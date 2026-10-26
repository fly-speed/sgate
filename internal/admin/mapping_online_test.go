package admin

import (
	"errors"
	"os"
	"testing"
)

func TestMappingSaveAppliesOnlineAndRollsBack(t *testing.T) {
	for _, mode := range []string{"live", "restart", "bind-failure"} {
		t.Run(mode, func(t *testing.T) {
			h, cfg := adminFixture(t)
			cookie, csrf := setupAccount(t, h, cfg)
			before, _ := os.ReadFile(h.view.AppConfigPath)
			before = append(before, []byte("master_service = 127.0.0.1|9000\n")...)
			os.WriteFile(h.view.AppConfigPath, before, 0600)
			h.view.LiveFields = []string{"proxies", "master_service"}
			calls := 0
			h.view.ApplyLive = func(keys []string) error {
				calls++
				if len(keys) != 2 {
					t.Error("mapping and framework listener not applied together", keys)
				}
				switch mode {
				case "restart":
					return ErrMappingServiceRestart
				case "bind-failure":
					return errors.New("address already in use")
				}
				return nil
			}
			h.startupRevision = h.restartDigest(before)
			body := map[string]any{"revision": digest(before), "fields": map[string]string{}, "proxies": []proxyMapping{{Listen: "127.0.0.1:9002", Server: "ws://localhost:8081/tunnel"}}}
			w := request(h, "PUT", "/api/config", body, cookie, csrf, "")
			if mode == "bind-failure" {
				after, _ := os.ReadFile(h.view.AppConfigPath)
				if w.Code != 409 || string(after) != string(before) {
					t.Fatal("mapping save was not rolled back", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != 200 || calls != 1 {
				t.Fatal(w.Code, w.Body.String(), calls)
			}
			data := object(t, w)
			if (data["service_restart_required"] == true) != (mode == "restart") {
				t.Fatal(data)
			}
			got := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
			if (got["requires_restart"] == true) != (mode == "restart") {
				t.Fatal("wrong pending state", got)
			}
		})
	}
}
