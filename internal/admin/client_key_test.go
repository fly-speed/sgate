package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientKeyEditingDoesNotExposeSecretAndChecksRevision(t *testing.T) {
	h, cfg := adminFixture(t)
	h.view.Role = "client"
	cookie, csrf := setupAccount(t, h, cfg)
	path := filepath.Join(t.TempDir(), "sgate.key")
	old, next := strings.Repeat("ab", 32), strings.Repeat("CD", 32)
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	h.view.ClientKeyPath = func() (string, error) { return path, nil }
	get := request(h, "GET", "/api/client-key", nil, cookie, "", "")
	if get.Code != http.StatusOK || strings.Contains(get.Body.String(), old) {
		t.Fatal("key metadata failed or exposed secret", get.Code)
	}
	rev := object(t, get)["revision"].(string)
	save := func(revision, key, token string) int {
		w := request(h, "PUT", "/api/client-key", map[string]string{"revision": revision, "key": key}, cookie, token, "")
		if strings.Contains(w.Body.String(), old) || strings.Contains(w.Body.String(), next) {
			t.Fatal("secret exposed")
		}
		return w.Code
	}
	if save(rev, next, "") != 403 || save("stale", next, csrf) != 409 ||
		save(rev, "invalid", csrf) != 400 || save(rev, next, csrf) != 200 || save(rev, old, csrf) != 409 {
		t.Fatal("unexpected save response")
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != strings.ToLower(next)+"\n" || !h.clientKeyPending {
		t.Fatal("key was not saved or marked pending")
	}
}
