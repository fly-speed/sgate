package admin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sgate/internal/tunnel"
)

func TestReverseListenerConfigTransaction(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{true: "rollback", false: "commit"}[fail], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sgate-server.cf")
			original := "service sgate-server {\n master_service = |1080, 127.0.0.1|10022 # keep comment\n target = 127.0.0.1:22\n}\n"
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			before := []TunnelAccountInfo{{ReverseMappings: []tunnel.ReverseMapping{{Listen: "127.0.0.1:10022", Target: "127.0.0.1:22"}}}}
			accounts := []tunnel.Account{{Username: "alice", Password: "secret", ReverseMappings: []tunnel.ReverseMapping{{Listen: "[::1]:18080", Target: "localhost:8080"}}}, {Username: "disabled", Disabled: true, ReverseMappings: []tunnel.ReverseMapping{{Listen: "127.0.0.1:2000", Target: "localhost:2"}}}}
			calls := 0
			address, changed, err := SyncReverseListeners(path, before, accounts, func(value string, changed bool) error {
				calls++
				saved, _ := os.ReadFile(path)
				if !changed || value != "|1080, [::1]|18080" || !strings.Contains(string(saved), "[::1]|18080") {
					t.Fatalf("apply before config persisted: %q %s", value, saved)
				}
				if fail {
					return errors.New("reject apply")
				}
				return nil
			})
			if calls != 1 || (err != nil) != fail || changed == fail {
				t.Fatal(calls, changed, err)
			}
			saved, _ := os.ReadFile(path)
			if fail {
				if string(saved) != original {
					t.Fatalf("failed transaction: %s", saved)
				}
			} else {
				if address != "|1080, [::1]|18080" || strings.Contains(string(saved), "10022") || strings.Contains(string(saved), "|2000") || !strings.Contains(string(saved), "# keep comment") {
					t.Fatal(string(saved))
				}
				_, changed, err = SyncReverseListeners(path, []TunnelAccountInfo{{ReverseMappings: accounts[0].ReverseMappings}}, nil, func(string, bool) error { return nil })
				if err != nil || !changed {
					t.Fatal(changed, err)
				}
				saved, _ = os.ReadFile(path)
				if strings.Contains(string(saved), "18080") || !strings.Contains(string(saved), "|1080") {
					t.Fatal(string(saved))
				}
			}
		})
	}
}

func TestReverseListenerRejectsHTTPPortConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.cf")
	original := []byte("service server {\n master_service = |1080\n}\n")
	os.WriteFile(path, original, 0600)
	_, _, err := SyncReverseListeners(path, nil, []tunnel.Account{{ReverseMappings: []tunnel.ReverseMapping{{Listen: "127.0.0.1:1080", Target: "localhost:22"}}}}, func(string, bool) error { t.Fatal("conflict applied"); return nil })
	if err == nil {
		t.Fatal("HTTP conflict accepted")
	}
	saved, _ := os.ReadFile(path)
	if string(saved) != string(original) {
		t.Fatal("conflict changed config")
	}
}

func TestDisabledReverseMappingDoesNotRemoveHTTPListener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.cf")
	original := []byte("service server {\n master_service = 127.0.0.1|1080\n}\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	mappings := []tunnel.ReverseMapping{{Listen: "127.0.0.1:1080", Target: "localhost:22"}}
	previous := []TunnelAccountInfo{{Disabled: true, ReverseMappings: mappings}}
	_, changed, err := SyncReverseListeners(path, previous, []tunnel.Account{{Disabled: true, ReverseMappings: mappings}}, func(string, bool) error { return nil })
	if !errors.Is(err, ErrReverseTunnelPortConflict) || changed {
		t.Fatal(changed, err)
	}
	saved, _ := os.ReadFile(path)
	if string(saved) != string(original) {
		t.Fatal("HTTP listener removed by disabled policy")
	}
}

func TestReverseTunnelPortConflictAcrossAddresses(t *testing.T) {
	for _, listeners := range []string{"127.0.0.1:1080", "0.0.0.0:1080", "[::]:1080", "|1080", "127.0.0.1:9000, [::1]|1080"} {
		for _, listen := range []string{"127.0.0.1:1080", "127.0.0.2:1080", "0.0.0.0:1080", "[::1]:1080", "[::]:1080"} {
			for _, disabled := range []bool{false, true} {
				accounts := []tunnel.Account{{Disabled: disabled, ReverseMappings: []tunnel.ReverseMapping{{Listen: listen, Target: "localhost:22"}}}}
				if err := ValidateReverseTunnelPorts(accounts, listeners); !errors.Is(err, ErrReverseTunnelPortConflict) {
					t.Fatalf("%s vs %s disabled=%t: %v", listen, listeners, disabled, err)
				}
				accounts[0].ReverseMappings[0].Listen = "127.0.0.1:10022"
				if err := ValidateReverseTunnelPorts(accounts, listeners); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
