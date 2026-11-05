package admin

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func externalWeb(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"index.html": "<!doctype html><title>external v1</title>",
		"app.css":    "body { color: navy; }",
		"i18n.js":    "const translations = {};",
		"app.js":     "console.log('external v1');",
	} {
		if e := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	return dir
}
func TestExternalWebFilesReloadWithoutRestart(t *testing.T) {
	old, c := adminFixture(t)
	c.WebDir = externalWeb(t)
	h, e := New(c, old.view)
	if e != nil {
		t.Fatal(e)
	}
	for route, name := range map[string]string{
		"/":               "index.html",
		"/assets/app.css": "app.css",
		"/assets/app.js":  "app.js",
		"/assets/i18n.js": "i18n.js",
	} {
		get := func() *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", route, nil))
			return w
		}
		first := get()
		if first.Code != 200 || first.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("external asset unavailable", route, first.Code)
		}
		updated := first.Body.String() + "\n/* changed on disk */"
		if e := atomicFile(filepath.Join(c.WebDir, name), []byte(updated)); e != nil {
			t.Fatal(e)
		}
		second := get()
		if second.Code != 200 || second.Body.String() != updated {
			t.Fatal("asset did not reload from disk", route)
		}
	}
	// Even a file in web-dir is not published unless explicitly routed.
	os.WriteFile(filepath.Join(c.WebDir, "private.conf"), []byte("secret"), 0600)
	for _, path := range []string{
		"/private.conf",
		"/assets/private.conf",
		"/assets/../private.conf",
		"/assets/../../users.json",
		"/index.html",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 || strings.Contains(w.Body.String(), "secret") {
			t.Fatal("unexpected file exposure", path, w.Code)
		}
	}
	if e := os.Remove(filepath.Join(c.WebDir, "app.js")); e != nil {
		t.Fatal(e)
	}
	w := request(h, "GET", "/assets/app.js", nil, nil, "", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("missing asset did not report error", w.Code)
	}
	if w := request(h, "GET", "/api/status", nil, nil, "", ""); w.Code != 200 {
		t.Fatal("missing UI file disabled API")
	}
}
func TestExternalWebStartupRejectsMissingOrUnsafeAssets(t *testing.T) {
	old, c := adminFixture(t)
	for _, mode := range []string{"missing", "directory", "symlink", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			c := c
			c.WebDir = externalWeb(t)
			path := filepath.Join(c.WebDir, "index.html")
			if e := os.Remove(path); e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "directory":
				os.Mkdir(path, 0700)
			case "symlink":
				os.Symlink(filepath.Join(c.WebDir, "app.js"), path)
			case "oversize":
				os.WriteFile(path, make([]byte, (1<<20)+1), 0600)
			}
			if _, e := New(c, old.view); e == nil || !strings.Contains(e.Error(), "web-dir") {
				t.Fatal("bad assets accepted", e)
			}
		})
	}
}
func TestExternalWebDirectoryConfigResolution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin.conf")
	for _, value := range []string{"", "web-dir = custom/ui\n"} {
		if e := os.WriteFile(path, []byte(value), 0600); e != nil {
			t.Fatal(e)
		}
		c, e := LoadConfig(path)
		if e != nil {
			t.Fatal(e)
		}
		want := filepath.Join(dir, "../web/admin")
		if value != "" {
			want = filepath.Join(dir, "custom/ui")
		}
		if c.WebDir != want {
			t.Fatalf("web-dir=%s want=%s", c.WebDir, want)
		}
	}
}

func TestExternalWebStartupPreservesFilesystemCause(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		dir := externalWeb(t)
		os.Remove(filepath.Join(dir, "index.html"))
		if e := validateWebDir(dir); !errors.Is(e, os.ErrNotExist) {
			t.Fatalf("missing-file cause lost: %v", e)
		}
	})
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprintf("permission/directory=%t", directory), func(t *testing.T) {
			if runtime.GOOS == "windows" || os.Geteuid() == 0 {
				t.Skip("requires Unix chmod enforcement")
			}
			dir := externalWeb(t)
			path, mode := filepath.Join(dir, "index.html"), os.FileMode(0600)
			if directory {
				path, mode = dir, 0700
			}
			if e := os.Chmod(path, 0); e != nil {
				t.Fatal(e)
			}
			defer os.Chmod(path, mode)
			if e := validateWebDir(dir); !errors.Is(e, os.ErrPermission) {
				t.Fatalf("permission cause lost: %v", e)
			}
		})
	}
}
