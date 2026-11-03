//go:build linux

package vpn

import (
	"net/netip"
	"strings"
	"testing"
)

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
