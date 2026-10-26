package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestProbeFailureDetailsMatchBothLogs(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint(secure), func(t *testing.T) {
			f := newCommandProbeFixture(t, secure)
			closed, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			bad := closed.Addr().String()
			closed.Close()
			f.server.allowedTargets[bad] = struct{}{}
			cfg := f.client.cfg
			cfg.Addr = bad
			client, err := NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			result := client.Probe(context.Background())
			if result.OK || result.Stage != "backend" || result.Target != bad || !strings.Contains(result.Error, bad) || !strings.Contains(result.Error, "refused") {
				t.Fatalf("missing backend error detail: %+v", result)
			}
			for _, logs := range []string{f.clientLogs.text(), f.serverLogs.text()} {
				if !strings.Contains(logs, result.TestID) || !strings.Contains(logs, result.Error) {
					t.Fatalf("error and test ID absent from log: %+v\n%s", result, logs)
				}
				if strings.Contains(logs, testPassword) {
					t.Fatal("credential leaked")
				}
			}
			for _, kind := range []string{"auth", "allowlist"} {
				cfg := f.client.cfg
				if kind == "auth" {
					cfg.Password = "wrong-password"
				} else {
					cfg.Addr = "127.0.0.1:1"
				}
				client, err := NewClient(cfg)
				if err != nil {
					t.Fatal(err)
				}
				result := client.Probe(context.Background())
				if result.OK || result.Detail == "" || result.Error == "" || !strings.Contains(f.serverLogs.text(), result.TestID) {
					t.Fatalf("missing %s diagnostics: %+v", kind, result)
				}
			}
		})
	}
}

func TestProbeErrorClassificationAndBounds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := ProbeResult{Stage: "command"}
	r.fail(ctx, errors.New("closed socket"))
	if r.Detail != "connectivity test canceled" || r.Error != "closed socket" {
		t.Fatal(r)
	}
	r = ProbeResult{Stage: "command"}
	r.fail(context.Background(), &net.DNSError{Err: "i/o timeout", IsTimeout: true})
	if !strings.Contains(r.Detail, "timed out waiting") || r.Error == "" {
		t.Fatal(r)
	}
	diagnostic := probeDiagnostic(strings.Repeat("a", 2000) + "\nsecret")
	if len(diagnostic) > 515 || strings.ContainsAny(diagnostic, "\r\n\x00") {
		t.Fatal("unbounded diagnostic")
	}
	bad := probeReply{Command: probeResultCommand, TestID: "test", Status: "failed", Reason: resetRefused, Error: "injected\nlog"}
	if validProbeReply(bad, "test") {
		t.Fatal("accepted control characters in diagnostic")
	}
}
