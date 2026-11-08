//go:build linux

package vpn

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

func ApplyNetwork(o NetworkOptions) (cleanup func(), err error) { return applyNetwork(o, runCommand) }
func applyNetwork(o NetworkOptions, run commandRunner) (cleanup func(), err error) {
	t := &networkTransaction{run: run, logf: o.Logf}
	defer func() {
		if err != nil {
			t.close()
		}
	}()
	ip := o.ClientIP
	if o.Server {
		ip = o.Network.Addr().Next()
	}
	if o.Server {
		existing, e := run("ip", "-4", "route", "show", o.Network.String())
		if e != nil {
			return nil, e
		}
		if existing != "" {
			return nil, errors.New("VPN subnet conflicts with an existing route")
		}
	}
	address := fmt.Sprintf("%s/%d", ip, o.Network.Bits())
	if err = t.step("ip", []string{"addr", "add", address, "dev", o.Interface}, t.reverse("ip", "addr", "del", address, "dev", o.Interface)); err != nil {
		return nil, err
	}
	if err = t.step("ip", []string{"link", "set", "dev", o.Interface, "mtu", strconv.Itoa(o.MTU), "up"}, nil); err != nil {
		return nil, err
	}
	if o.Server {
		egress := o.Egress
		if egress == "" {
			line, e := run("ip", "-4", "route", "get", "1.1.1.1")
			if e != nil {
				return nil, e
			}
			fields := strings.Fields(line)
			for i := 0; i+1 < len(fields); i++ {
				if fields[i] == "dev" {
					egress = fields[i+1]
					break
				}
			}
		}
		if egress == "" || egress == o.Interface {
			return nil, errors.New("cannot determine VPN server egress interface")
		}
		old, e := run("sysctl", "-n", "net.ipv4.ip_forward")
		if e != nil {
			return nil, e
		}
		if old != "0" && old != "1" {
			return nil, errors.New("invalid IPv4 forwarding state")
		}
		if old == "0" {
			if err = t.step("sysctl", []string{"-w", "net.ipv4.ip_forward=1"}, t.reverse("sysctl", "-w", "net.ipv4.ip_forward=0")); err != nil {
				return nil, err
			}
		}
		input := []string{"-w", "5", "-I", "INPUT", "1", "-i", o.Interface, "-s", o.Network.String(), "-d", ip.String(), "-p", "icmp", "--icmp-type", "echo-request", "-j", "ACCEPT"}
		undoInput := []string{"-w", "5", "-D", "INPUT", "-i", o.Interface, "-s", o.Network.String(), "-d", ip.String(), "-p", "icmp", "--icmp-type", "echo-request", "-j", "ACCEPT"}
		if err = t.step("iptables", input, t.reverse("iptables", undoInput...)); err != nil {
			return nil, err
		}
		chain := fmt.Sprintf("SGATEVPN%d", os.Getpid())
		if err = t.step("iptables", []string{"-w", "5", "-N", chain}, t.reverse("iptables", "-w", "5", "-X", chain)); err != nil {
			return nil, err
		}
		// Flush only our new chain before deleting it during rollback.
		t.undo = append(t.undo, t.reverse("iptables", "-w", "5", "-F", chain))
		rules := [][]string{
			{"-A", chain, "-i", o.Interface, "-o", egress, "-s", o.Network.String(), "-m", "conntrack", "--ctstate", "NEW,ESTABLISHED,RELATED", "-j", "ACCEPT"},
			{"-A", chain, "-i", egress, "-o", o.Interface, "-d", o.Network.String(), "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
			{"-A", chain, "-j", "DROP"},
		}
		for _, rule := range rules {
			if err = t.step("iptables", append([]string{"-w", "5"}, rule...), nil); err != nil {
				return nil, err
			}
		}
		for _, direction := range []string{"-i", "-o"} {
			if err = t.step("iptables", []string{"-w", "5", "-I", "FORWARD", "1", direction, o.Interface, "-j", chain}, t.reverse("iptables", "-w", "5", "-D", "FORWARD", direction, o.Interface, "-j", chain)); err != nil {
				return nil, err
			}
		}
		if err = t.step("iptables", []string{"-w", "5", "-t", "nat", "-N", chain}, t.reverse("iptables", "-w", "5", "-t", "nat", "-X", chain)); err != nil {
			return nil, err
		}
		t.undo = append(t.undo, t.reverse("iptables", "-w", "5", "-t", "nat", "-F", chain))
		if err = t.step("iptables", []string{"-w", "5", "-t", "nat", "-A", chain, "-s", o.Network.String(), "-o", egress, "-j", "MASQUERADE"}, nil); err != nil {
			return nil, err
		}
		if err = t.step("iptables", []string{"-w", "5", "-t", "nat", "-I", "POSTROUTING", "1", "-s", o.Network.String(), "-o", egress, "-j", chain}, t.reverse("iptables", "-w", "5", "-t", "nat", "-D", "POSTROUTING", "-s", o.Network.String(), "-o", egress, "-j", chain)); err != nil {
			return nil, err
		}
	} else {
		line, e := run("ip", "-4", "route", "get", o.Endpoint.String())
		if e != nil {
			return nil, e
		}
		var gateway, dev string
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			switch fields[i] {
			case "via":
				gateway = fields[i+1]
			case "dev":
				dev = fields[i+1]
			}
		}
		if dev == "" || interfaceUnsafe(dev) {
			return nil, errors.New("VPN endpoint must use a physical route; stop other VPNs first")
		}
		existing, e := run("ip", "-4", "route", "show", "exact", o.Endpoint.String()+"/32")
		if e != nil {
			return nil, e
		}
		if existing == "" {
			args := []string{"route", "add", o.Endpoint.String() + "/32"}
			if gateway != "" {
				args = append(args, "via", gateway)
			}
			args = append(args, "dev", dev)
			undo := append([]string(nil), args...)
			undo[1] = "del"
			restore := t.reverse("ip", undo...)
			if r := o.recovery; r != nil {
				next, _ := netip.ParseAddr(gateway)
				if err = r.rememberRoute(o.Endpoint, next, dev, 0); err != nil {
					return nil, err
				}
				restore = recordCleanup(restore, r.clearRoute)
			}
			if err = t.step("ip", args, restore); err != nil {
				return nil, err
			}
		}
		for _, route := range o.Routes {
			args := []string{"route", "add", route.String(), "dev", o.Interface}
			if err = t.step("ip", args, t.reverse("ip", "route", "del", route.String(), "dev", o.Interface)); err != nil {
				return nil, err
			}
		}
		for _, dns := range o.DNS {
			covered := false
			for _, route := range o.Routes {
				covered = covered || route.Contains(dns)
			}
			if !covered {
				if err = t.step("ip", []string{"route", "add", dns.String() + "/32", "dev", o.Interface}, t.reverse("ip", "route", "del", dns.String()+"/32", "dev", o.Interface)); err != nil {
					return nil, err
				}
			}
		}
		if o.BlockIPv6 {
			// New devices inherit default.disable_ipv6. IPv6 routes cannot use
			// a disabled device, even when their purpose is to discard traffic.
			// Enable only our own TUN; leave global/physical interface settings
			// untouched. Closing the owned TUN removes this setting and routes.
			key := "net/ipv6/conf/" + o.Interface + "/disable_ipv6"
			disabled, e := run("sysctl", "-n", key)
			if e != nil {
				return nil, fmt.Errorf("inspect VPN IPv6 blocking on %s: %w", o.Interface, e)
			}
			if disabled == "1" {
				if err = t.step("sysctl", []string{"-w", key + "=0"}, nil); err != nil {
					return nil, fmt.Errorf("configure VPN IPv6 blocking on %s: %w", o.Interface, err)
				}
			}
			for _, route := range []string{"::/1", "8000::/1"} {
				if err = t.step("ip", []string{"-6", "route", "add", route, "dev", o.Interface}, t.reverse("ip", "-6", "route", "del", route, "dev", o.Interface)); err != nil {
					return nil, err
				}
			}
		}
		if len(o.DNS) > 0 {
			args := append([]string{"dns", o.Interface}, dnsStrings(o.DNS)...)
			if err = t.step("resolvectl", args, t.reverse("resolvectl", "revert", o.Interface)); err != nil {
				return nil, err
			}
			if err = t.step("resolvectl", []string{"domain", o.Interface, "~."}, nil); err != nil {
				return nil, err
			}
		}
	}
	return t.close, nil
}
