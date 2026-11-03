//go:build linux

package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Used only inside the temporary namespace created by the benchmark driver.
func configureNetem(t *testing.T, endpoint, mode string) {
	t.Helper()
	token := os.Getenv("SGATE_BASELINE_NETNS")
	if !strings.HasPrefix(token, "sgate-bench-") || len(token) > 64 || strings.ContainsAny(token, "/\\. \n") {
		t.Fatal("netem requires an isolated benchmark namespace")
	}
	current, err := os.Stat("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.Stat(filepath.Join("/var/run/netns", token))
	if err != nil || !os.SameFile(current, expected) {
		t.Fatal("refusing to change host network qdisc")
	}
	var p impairmentProfile
	if json.Unmarshal([]byte(os.Getenv("SGATE_BASELINE_IMPAIRMENT")), &p) != nil {
		t.Fatal("invalid netem profile")
	}
	if p.Delay < 0 || p.Delay > 2*time.Second || p.DropEvery < 0 || p.BytesPerSecond < 0 {
		t.Fatal("invalid netem range")
	}
	_, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		t.Fatal("invalid shaped tunnel port")
	}
	run := func(name string, args ...string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("netem setup %s: %v: %s", name, err, out)
		}
	}
	mtu := 1500
	if p.MaxDatagram > 0 {
		mtu = p.MaxDatagram + 28
	}
	if mtu < 1228 || mtu > 65535 {
		t.Fatal("MTU must support 1200-byte QUIC payload")
	}
	run("ip", "link", "set", "lo", "mtu", strconv.Itoa(mtu))
	run("ethtool", "-K", "lo", "tso", "off", "gso", "off", "gro", "off")
	run("tc", "qdisc", "add", "dev", "lo", "root", "handle", "1:", "prio", "bands", "3", "priomap", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0")
	args := []string{"qdisc", "add", "dev", "lo", "parent", "1:3", "handle", "30:", "netem", "limit", "1000"}
	if p.Delay > 0 {
		args = append(args, "delay", strconv.FormatFloat(float64(p.Delay)/float64(time.Millisecond), 'f', 3, 64)+"ms")
	}
	if p.DropEvery > 0 {
		args = append(args, "loss", strconv.FormatFloat(100/float64(p.DropEvery), 'f', 6, 64)+"%")
	}
	if p.ReorderEvery > 0 {
		if p.Delay == 0 {
			t.Fatal(errors.New("netem reordering requires delay"))
		}
		args = append(args, "reorder", strconv.FormatFloat(100/float64(p.ReorderEvery), 'f', 6, 64)+"%")
	}
	if p.BytesPerSecond > 0 {
		args = append(args, "rate", strconv.Itoa(p.BytesPerSecond*8)+"bit")
	}
	run("tc", args...)
	protocol := "17"
	if mode == "wss" {
		protocol = "6"
	}
	for _, field := range []string{"sport", "dport"} {
		run("tc", "filter", "add", "dev", "lo", "protocol", "ip", "parent", "1:", "prio", "1", "u32", "match", "ip", "protocol", protocol, "0xff", "match", "ip", field, port, "0xffff", "flowid", "1:3")
	}
}
