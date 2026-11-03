//go:build darwin

package vpn

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

func ApplyNetwork(o NetworkOptions) (cleanup func(), err error) { return applyNetwork(o, runCommand) }
func applyNetwork(o NetworkOptions, run commandRunner) (cleanup func(), err error) {
	if o.Server {
		return nil, errors.New("VPN server network forwarding currently requires Linux")
	}
	t := &networkTransaction{run: run, logf: o.Logf}
	defer func() {
		if err != nil {
			t.close()
		}
	}()
	line, e := run("route", "-n", "get", o.Endpoint.String())
	if e != nil {
		return nil, e
	}
	var gateway, dev, destination string
	for _, line := range strings.Split(line, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok {
			switch key {
			case "gateway":
				gateway = strings.TrimSpace(value)
			case "interface":
				dev = strings.TrimSpace(value)
			case "destination":
				destination = strings.TrimSpace(value)
			}
		}
	}
	if dev == "" || interfaceUnsafe(dev) {
		return nil, errors.New("VPN endpoint must use a physical route; stop other VPNs first")
	}
	if destination != o.Endpoint.String() {
		if _, e = netip.ParseAddr(gateway); e == nil {
			if err = t.step("route", []string{"-n", "add", "-host", o.Endpoint.String(), gateway}, t.reverse("route", "-n", "delete", "-host", o.Endpoint.String(), gateway)); err != nil {
				return nil, err
			}
		}
	}
	gatewayIP := o.Network.Addr().Next()
	if err = t.step("ifconfig", []string{o.Interface, "inet", o.ClientIP.String(), gatewayIP.String(), "netmask", "255.255.255.255", "mtu", strconv.Itoa(o.MTU), "up"}, nil); err != nil {
		return nil, err
	}
	for _, route := range o.Routes {
		if err = t.step("route", []string{"-n", "add", "-net", route.String(), "-interface", o.Interface}, t.reverse("route", "-n", "delete", "-net", route.String(), "-interface", o.Interface)); err != nil {
			return nil, err
		}
	}
	for _, dns := range o.DNS {
		covered := false
		for _, route := range o.Routes {
			covered = covered || route.Contains(dns)
		}
		if !covered {
			if err = t.step("route", []string{"-n", "add", "-host", dns.String(), "-interface", o.Interface}, t.reverse("route", "-n", "delete", "-host", dns.String(), "-interface", o.Interface)); err != nil {
				return nil, err
			}
		}
	}
	if o.BlockIPv6 {
		for _, route := range []string{"::/1", "8000::/1"} {
			if err = t.step("route", []string{"-n", "add", "-inet6", "-net", route, "-interface", o.Interface}, t.reverse("route", "-n", "delete", "-inet6", "-net", route, "-interface", o.Interface)); err != nil {
				return nil, err
			}
		}
	}
	if len(o.DNS) > 0 {
		if o.DNSService == "" {
			return nil, errors.New("macOS DNS requires dns-service, e.g. Wi-Fi")
		}
		previous, e := run("networksetup", "-getdnsservers", o.DNSService)
		if e != nil {
			return nil, e
		}
		old := strings.Fields(previous)
		if strings.HasPrefix(previous, "There aren't any DNS Servers set on ") {
			old = []string{"Empty"}
		} else {
			for _, value := range old {
				if _, e = netip.ParseAddr(value); e != nil {
					return nil, fmt.Errorf("cannot parse previous DNS configuration for %s", o.DNSService)
				}
			}
			if len(old) == 0 {
				return nil, errors.New("empty DNS service response")
			}
		}
		args := append([]string{"-setdnsservers", o.DNSService}, dnsStrings(o.DNS)...)
		undo := append([]string{"-setdnsservers", o.DNSService}, old...)
		if err = t.step("networksetup", args, t.reverse("networksetup", undo...)); err != nil {
			return nil, err
		}
	}
	return t.close, nil
}
