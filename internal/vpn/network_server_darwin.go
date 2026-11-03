//go:build darwin

package vpn

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var macInterfaceName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,31}$`)
var macTUNName = regexp.MustCompile(`^utun[0-9]+$`)
var pfTokenPattern = regexp.MustCompile(`(?m)^\s*Token\s*:\s*([0-9]+)\s*$`)

// Use the standard macOS wildcard hooks, with a unique direct child anchor.
// Never reload /etc/pf.conf or flush the main ruleset / shared state table.
func applyMacServerNetwork(o NetworkOptions, run commandRunner) (cleanup func(), err error) {
	if !macTUNName.MatchString(o.Interface) || !o.Network.IsValid() || !o.Network.Addr().Is4() || o.Network != o.Network.Masked() || o.Network.Bits() < 16 || o.Network.Bits() > 30 || o.MTU < 576 || o.MTU > 1500 {
		return nil, errors.New("invalid macOS VPN server interface, IPv4 network or MTU")
	}
	egress := o.Egress
	if egress == "" {
		route, e := run("route", "-n", "get", "1.1.1.1")
		if e != nil {
			return nil, e
		}
		for _, line := range strings.Split(route, "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
			if ok && key == "interface" {
				egress = strings.TrimSpace(value)
			}
		}
	}
	if !macInterfaceName.MatchString(egress) || interfaceUnsafe(egress) || egress == "lo0" || egress == o.Interface {
		return nil, errors.New("VPN server egress must be a physical macOS interface, e.g. en0; stop other full-tunnel VPNs or set vpn-egress-interface")
	}
	if _, e := run("ifconfig", egress); e != nil {
		return nil, fmt.Errorf("VPN egress interface unavailable: %w", e)
	}
	nat, e := run("pfctl", "-s", "nat")
	if e != nil {
		return nil, fmt.Errorf("read PF NAT rules (root required): %w", e)
	}
	filter, e := run("pfctl", "-s", "rules")
	if e != nil {
		return nil, fmt.Errorf("read PF filter rules (root required): %w", e)
	}
	if !hasMacPFHook(nat, "nat-anchor") || !hasMacPFHook(filter, "anchor") {
		return nil, errors.New("macOS PF main rules need nat-anchor and anchor hooks for com.apple/*; automatic VPN setup will not reload the main ruleset; configure these hooks or use vpn-manage-network=false")
	}
	previous, e := run("sysctl", "-n", "net.inet.ip.forwarding")
	if e != nil {
		return nil, e
	}
	if previous != "0" && previous != "1" {
		return nil, errors.New("invalid macOS IPv4 forwarding state")
	}
	nonce := make([]byte, 8)
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	tag := "SGATEVPN" + strconv.Itoa(os.Getpid()) + hex.EncodeToString(nonce)
	anchor := "com.apple/sgate-vpn-" + strconv.Itoa(os.Getpid()) + "-" + hex.EncodeToString(nonce)
	rules := macServerPFRules(o, egress, tag)
	file, e := os.CreateTemp("", "sgate-vpn-pf-*.conf")
	if e != nil {
		return nil, e
	}
	path := file.Name()
	t := &networkTransaction{run: run, logf: o.Logf}
	t.undo = append(t.undo, func() error { return os.Remove(path) })
	defer func() {
		if err != nil {
			t.close()
		}
	}()
	if _, err = file.WriteString(rules); err != nil {
		file.Close()
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	if _, err = run("pfctl", "-n", "-a", anchor, "-f", path); err != nil {
		return nil, fmt.Errorf("VPN PF rules failed syntax validation: %w", err)
	}
	gateway := o.Network.Addr().Next().String()
	if err = t.step("ifconfig", []string{o.Interface, "inet", gateway, gateway, "netmask", "255.255.255.255", "mtu", strconv.Itoa(o.MTU), "up"}, t.reverse("ifconfig", o.Interface, "inet", gateway, "-alias")); err != nil {
		return nil, err
	}
	if err = t.step("route", []string{"-n", "add", "-net", o.Network.String(), "-interface", o.Interface}, t.reverse("route", "-n", "delete", "-net", o.Network.String(), "-interface", o.Interface)); err != nil {
		return nil, fmt.Errorf("VPN subnet route unavailable or conflicting: %w", err)
	}
	enabled, e := run("pfctl", "-E")
	if e != nil {
		return nil, fmt.Errorf("enable PF for VPN: %w", e)
	}
	token := pfTokenPattern.FindStringSubmatch(enabled)
	if len(token) != 2 {
		return nil, errors.New("PF enabled but returned no reference token; cannot safely release it automatically; inspect pfctl -s References; main PF state was not disabled")
	}
	t.undo = append(t.undo, t.reverse("pfctl", "-X", token[1]))
	// Record cleanup before loading so partial load failures also clear our anchor.
	t.undo = append(t.undo, func() error {
		_, natErr := run("pfctl", "-a", anchor, "-F", "nat")
		_, filterErr := run("pfctl", "-a", anchor, "-F", "rules")
		return errors.Join(natErr, filterErr)
	})
	if _, err = run("pfctl", "-a", anchor, "-f", path); err != nil {
		return nil, fmt.Errorf("load VPN PF anchor: %w", err)
	}
	if previous == "0" {
		if err = t.step("sysctl", []string{"-w", "net.inet.ip.forwarding=1"}, t.reverse("sysctl", "-w", "net.inet.ip.forwarding=0")); err != nil {
			return nil, err
		}
	}
	if o.Logf != nil {
		o.Logf("VPN macOS network: interface=%s egress=%s PF anchor=%s", o.Interface, egress, anchor)
	}
	return t.close, nil
}
func hasMacPFHook(output, kind string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == kind && fields[1] == `"com.apple/*"` {
			return true
		}
	}
	return false
}
func macServerPFRules(o NetworkOptions, egress, tag string) string {
	// NAT runs before filtering; use a unique tag to identify this VPN's
	// translated packets without allowing unrelated traffic on the egress.
	return fmt.Sprintf("nat on %s inet from %s to any tag %s -> (%s)\n"+
		"pass in quick on %s inet from %s to any tag %s keep state (floating)\n"+
		"pass out quick on %s inet all tagged %s keep state (floating)\n"+
		"pass out quick on %s inet from any to %s keep state (floating)\n"+
		"block drop quick on %s all\n", egress, o.Network, tag, egress, o.Interface, o.Network, tag, egress, tag, o.Interface, o.Network, o.Interface)
}
