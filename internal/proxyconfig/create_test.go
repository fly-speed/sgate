package proxyconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCreatePublishesCompletePrivateFileAndPreservesWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "routes.json")
	var wins atomic.Int32
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			data := []byte(fmt.Sprintf(`[{"listen":"127.0.0.1:9001","server":"ws://localhost/tunnel","username":"user-%d","password":"test-secret"}]`, i))
			created, err := Create(path, data)
			if err != nil {
				t.Error(err)
				return
			}
			if created {
				wins.Add(1)
			}
		}(i)
	}
	group.Wait()
	if wins.Load() != 1 {
		t.Fatal("exclusive creation failed", wins.Load())
	}
	raw, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := Decode(raw); err != nil || len(parsed) != 1 {
		t.Fatal("incomplete mapping file published", err)
	}
	for file, mode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
		st, err := os.Stat(file)
		if err != nil || st.Mode().Perm() != mode {
			t.Fatal("incorrect storage permissions", file, err)
		}
	}
	if created, err := Create(path, []byte("[]\n")); err != nil || created {
		t.Fatal("existing file overwritten", created, err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(raw) {
		t.Fatal("existing content changed")
	}
	temps, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".sgate-config-*"))
	if len(temps) > 0 {
		t.Fatal("temporary files retained", temps)
	}
}

func TestCreateRejectsInvalidDataAndUnwritableDirectory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "new", "routes.json")
	if _, err := Create(path, []byte("not-json")); err == nil {
		t.Fatal("invalid data accepted")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("invalid data created a directory")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	if err := os.Chmod(root, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0700)
	if _, err := Create(filepath.Join(root, "routes.json"), []byte("[]\n")); err == nil {
		t.Fatal("unwritable directory reported success")
	}
}
