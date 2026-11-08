//go:build linux

package vpn

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestLinuxClientIPv6BlockingWithDisabledDevice(t *testing.T) {
	for _, tc := range []struct {
		name, disabled, fail string
		block                bool
	}{
		{"inherited-disabled", "1", "", true},
		{"already-enabled", "0", "", true},
		{"blocking-off", "1", "", false},
		{"enable-failure", "1", "sysctl -w", true},
		{"route-failure", "1", "ip -6 route add 8000::/1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var commands []string
			run := func(name string, args ...string) (string, error) {
				line := name + " " + strings.Join(args, " ")
				commands = append(commands, line)
				if tc.fail != "" && strings.HasPrefix(line, tc.fail) {
					return "", errors.New("test failure")
				}
				switch line {
				case "ip -4 route get 192.0.2.1":
					return "192.0.2.1 via 192.0.2.254 dev eth0", nil
				case "sysctl -n net/ipv6/conf/sgate1/disable_ipv6":
					return tc.disabled, nil
				}
				return "", nil
			}
			o := NetworkOptions{Interface: "sgate1", Network: netip.MustParsePrefix("10.77.0.0/24"), ClientIP: netip.MustParseAddr("10.77.0.2"), MTU: 1280, Endpoint: netip.MustParseAddr("192.0.2.1"), Routes: []netip.Prefix{netip.MustParsePrefix("198.18.0.0/24")}, BlockIPv6: tc.block}
			undo, err := applyNetwork(o, run)
			if (err != nil) != (tc.fail != "") {
				t.Fatalf("unexpected setup result: %v", err)
			}
			if undo != nil {
				undo()
			}
			joined := strings.Join(commands, "\n")
			const enable = "sysctl -w net/ipv6/conf/sgate1/disable_ipv6=0"
			if strings.Contains(joined, enable) != (tc.block && tc.disabled == "1") {
				t.Fatal("unexpected IPv6 setting change", joined)
			}
			if tc.block && tc.fail == "" {
				for _, prefix := range []string{"::/1", "8000::/1"} {
					add := "ip -6 route add " + prefix + " dev sgate1"
					if !strings.Contains(joined, add) || !strings.Contains(joined, "ip -6 route del "+prefix+" dev sgate1") {
						t.Fatal("blocking routes not installed/cleaned", joined)
					}
					if tc.disabled == "1" && strings.Index(joined, enable) > strings.Index(joined, add) {
						t.Fatal("IPv6 enabled after route setup")
					}
				}
			}
			if strings.Contains(tc.fail, "8000::/1") && !strings.Contains(joined, "ip -6 route del ::/1 dev sgate1") {
				t.Fatal("partial IPv6 routes not rolled back", joined)
			}
			if !strings.Contains(joined, "ip addr del 10.77.0.2/24 dev sgate1") || !strings.Contains(joined, "ip route del 198.18.0.0/24 dev sgate1") {
				t.Fatal("network setup not cleaned", joined)
			}
			for _, command := range commands {
				if strings.HasPrefix(command, "sysctl -w ") && command != enable {
					t.Fatal("changed another interface/global IPv6 setting", command)
				}
				if !tc.block && strings.HasPrefix(command, "ip -6 ") {
					t.Fatal("IPv6 routes installed with blocking off")
				}
			}
		})
	}
}

func TestLinuxNetworkOwnsOnlyItsFirewallRules(t *testing.T) {
	var commands []string
	run := func(name string, args ...string) (string, error) {
		line := name + " " + strings.Join(args, " ")
		commands = append(commands, line)
		if name == "sysctl" && args[0] == "-n" {
			return "0", nil
		}
		return "", nil
	}
	o := NetworkOptions{Server: true, Interface: "sgate0", Network: netip.MustParsePrefix("10.77.0.0/24"), MTU: 1280, Egress: "eth0"}
	undo, err := applyNetwork(o, run)
	if err != nil {
		t.Fatal(err)
	}
	undo()
	joined := strings.Join(commands, "\n")
	if !strings.Contains(joined, "net.ipv4.ip_forward=0") || !strings.Contains(joined, "-D POSTROUTING") || !strings.Contains(joined, "-D FORWARD") {
		t.Fatal("incomplete cleanup", joined)
	}
	for _, line := range commands {
		if strings.Contains(line, "-F") && !strings.Contains(line, "SGATEVPN") {
			t.Fatal("flushed unrelated firewall state", line)
		}
	}
}
