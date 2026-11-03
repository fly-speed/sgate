package vpn

import (
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"time"
)

type NetworkOptions struct {
	Server     bool
	Interface  string
	Network    netip.Prefix
	ClientIP   netip.Addr
	MTU        int
	Endpoint   netip.Addr
	Routes     []netip.Prefix
	DNS        []netip.Addr
	DNSService string
	Egress     string
	BlockIPv6  bool
	Logf       func(string, ...any)
}
type commandRunner func(string, ...string) (string, error)

func runCommand(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %.1000s", name, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

type networkTransaction struct {
	run  commandRunner
	undo []func() error
	logf func(string, ...any)
}

func (t *networkTransaction) step(name string, args []string, undo func() error) error {
	if _, err := t.run(name, args...); err != nil {
		return err
	}
	if undo != nil {
		t.undo = append(t.undo, undo)
	}
	return nil
}
func (t *networkTransaction) close() {
	for i := len(t.undo) - 1; i >= 0; i-- {
		if err := t.undo[i](); err != nil && t.logf != nil {
			t.logf("VPN network cleanup: %v", err)
		}
	}
	t.undo = nil
}
func (t *networkTransaction) reverse(name string, args ...string) func() error {
	return func() error { _, err := t.run(name, args...); return err }
}
func dnsStrings(v []netip.Addr) []string {
	out := make([]string, len(v))
	for i, ip := range v {
		out[i] = ip.String()
	}
	return out
}
func interfaceUnsafe(name string) bool {
	return strings.HasPrefix(name, "utun") || strings.HasPrefix(name, "tun") || strings.HasPrefix(name, "wg") || strings.HasPrefix(name, "sgate")
}
