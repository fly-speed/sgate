package admin

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
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

func TestVPNClientFileImportStoresPrivateKeyWithoutChangingConfiguration(t *testing.T) {
	h, cfg := adminFixture(t)
	cookie, csrf := setupAccount(t, h, cfg)
	original, err := os.ReadFile(h.view.AppConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("AB", 32)
	importFile := func(kind, content, token string) *httptest.ResponseRecorder {
		return request(h, "POST", "/api/vpn-client-file", map[string]string{"kind": kind, "content": content}, cookie, token, "")
	}
	if importFile("key", key, "").Code != 403 || importFile("key", "invalid", csrf).Code != 400 ||
		importFile("ca", "invalid", csrf).Code != 400 {
		t.Fatal("invalid import was accepted")
	}
	for range 2 {
		w := importFile("key", key+"\n", csrf)
		if w.Code != 200 || strings.Contains(strings.ToLower(w.Body.String()), strings.ToLower(key)) {
			t.Fatalf("import response status %d or secret exposed", w.Code)
		}
		path := object(t, w)["path"].(string)
		if filepath.Dir(path) != filepath.Dir(h.view.AppConfigPath) {
			t.Fatal("key stored outside config directory")
		}
		if content, err := os.ReadFile(path); err != nil || string(content) != strings.ToLower(key)+"\n" {
			t.Fatal("key was not imported")
		}
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(h.view.AppConfigPath), "vpn-client-*.key"))
	if err != nil || len(files) != 2 {
		t.Fatal("each import should create its own file", err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, public, private)
	if err != nil {
		t.Fatal(err)
	}
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	w := importFile("ca", ca, csrf)
	if w.Code != 200 {
		t.Fatalf("CA import status %d: %s", w.Code, w.Body.String())
	}
	if content, err := os.ReadFile(object(t, w)["path"].(string)); err != nil || string(content) != ca {
		t.Fatal("CA was not imported")
	}
	if content, err := os.ReadFile(h.view.AppConfigPath); err != nil || string(content) != string(original) {
		t.Fatal("import changed the active configuration")
	}
}
