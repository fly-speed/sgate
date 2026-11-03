//go:build darwin

package vpn

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestMacNetworkRollbackAndDNSRestore(t *testing.T) {
	for _, fail := range []bool{false, true} {
		var commands []string
		runner := func(name string, args ...string) (string, error) {
			line := name + " " + strings.Join(args, " ")
			commands = append(commands, line)
			if name == "route" && len(args) > 1 && args[1] == "get" {
				return "destination: default\ngateway: 192.168.1.1\ninterface: en0", nil
			}
			if name == "networksetup" && args[0] == "-getdnsservers" {
				return "8.8.8.8\n8.8.4.4", nil
			}
			if fail && name == "networksetup" && args[0] == "-setdnsservers" && args[2] == "1.1.1.1" {
				return "", errors.New("injected DNS failure")
			}
			return "", nil
		}
		o := NetworkOptions{Interface: "utun99", Network: netip.MustParsePrefix("10.77.0.0/24"), ClientIP: netip.MustParseAddr("10.77.0.2"), MTU: 1280, Endpoint: netip.MustParseAddr("192.0.2.1"), Routes: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/1"), netip.MustParsePrefix("128.0.0.0/1")}, DNS: []netip.Addr{netip.MustParseAddr("1.1.1.1")}, DNSService: "Wi-Fi", BlockIPv6: true}
		undo, err := applyNetwork(o, runner)
		if fail && err == nil {
			t.Fatal("failure swallowed")
		}
		if !fail {
			if err != nil {
				t.Fatal(err)
			}
			undo()
		}
		joined := strings.Join(commands, "\n")
		for _, expected := range []string{"route -n delete -net 0.0.0.0/1 -interface utun99", "route -n delete -inet6 -net ::/1 -interface utun99", "route -n delete -host 192.0.2.1 192.168.1.1"} {
			if !strings.Contains(joined, expected) {
				t.Fatal("missing rollback", expected, joined)
			}
		}
		if !fail && !strings.Contains(joined, "networksetup -setdnsservers Wi-Fi 8.8.8.8 8.8.4.4") {
			t.Fatal("previous DNS not restored", joined)
		}
		if strings.Contains(joined, "delete default") || strings.Contains(joined, "change default") {
			t.Fatal("modified default route")
		}
	}
}
func TestMacNetworkRejectsExistingVPNAndPreservesEndpointRoute(t *testing.T) {
	for _, dev := range []string{"utun6", "en0"} {
		var commands []string
		run := func(name string, args ...string) (string, error) {
			commands = append(commands, name+" "+strings.Join(args, " "))
			if name == "route" && args[1] == "get" {
				return "destination: 192.0.2.1\ngateway: 192.168.1.1\ninterface: " + dev, nil
			}
			return "", nil
		}
		o := NetworkOptions{Interface: "utun99", Network: netip.MustParsePrefix("10.77.0.0/24"), ClientIP: netip.MustParseAddr("10.77.0.2"), MTU: 1280, Endpoint: netip.MustParseAddr("192.0.2.1")}
		undo, err := applyNetwork(o, run)
		if dev == "utun6" {
			if err == nil {
				t.Fatal("tunnel recursion allowed")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			undo()
		}
		if strings.Contains(strings.Join(commands, "\n"), "-host 192.0.2.1") {
			t.Fatal("existing endpoint route overwritten")
		}
	}
}

func TestMacNetworkRestoresAutomaticDNS(t *testing.T) {
	var commands []string
	run := func(name string, args ...string) (string, error) {
		line := name + " " + strings.Join(args, " ")
		commands = append(commands, line)
		if name == "route" && args[1] == "get" {
			return "destination: 192.0.2.1\ngateway: 192.168.1.1\ninterface: en0", nil
		}
		if name == "networksetup" && args[0] == "-getdnsservers" {
			return "There aren't any DNS Servers set on Wi-Fi.", nil
		}
		return "", nil
	}
	o := NetworkOptions{Interface: "utun99", Network: netip.MustParsePrefix("10.77.0.0/24"), ClientIP: netip.MustParseAddr("10.77.0.2"), MTU: 1280, Endpoint: netip.MustParseAddr("192.0.2.1"), DNS: []netip.Addr{netip.MustParseAddr("1.1.1.1")}, DNSService: "Wi-Fi"}
	undo, err := applyNetwork(o, run)
	if err != nil {
		t.Fatal(err)
	}
	undo()
	if !strings.Contains(strings.Join(commands, "\n"), "networksetup -setdnsservers Wi-Fi Empty") {
		t.Fatal("automatic DNS not restored", commands)
	}
}
