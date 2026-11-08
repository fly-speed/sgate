package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSavedMasterArgumentsAreValidatedBeforeRestart(t *testing.T) {
	for _, role := range []string{"client", "server"} {
		t.Run(role, func(t *testing.T) {
			body := "target = 127.0.0.1:1\n"
			if role == "client" {
				body = testClientMapping(t, "ws://127.0.0.1:1/tunnel")
			}
			c := newController(t, role, body)
			original, err := os.ReadFile(c.path)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(filepath.Dir(c.path), "candidate.cf")
			for _, raw := range []string{"-unknown-option value", "-noise-max-bytes 11M", "-heartbeat 2", "-noise-interval 1s -noise-interval-min 1s -noise-interval-max 3s"} {
				if err = os.WriteFile(path, []byte("service audit {\n"+string(original)+"master_args = "+raw+"\n}\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err = c.load(path); err == nil {
					t.Fatal("invalid master_args accepted", role)
				}
			}
			if err = os.WriteFile(path, []byte("service audit {\n"+string(original)+"master_args = -noise-max-bytes 10M\n}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = c.load(path); err != nil {
				t.Fatal("valid future overrides rejected", role, err)
			}
		})
	}
}
