package configfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateDirectoryCanReopenWithSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := PrivateDirectory(path); err != nil {
		t.Fatal(err)
	}
	if created, err := CreatePrivate(filepath.Join(path, "session"), []byte("example")); err != nil || !created {
		t.Fatalf("create session: %t, %v", created, err)
	}
	if err := PrivateDirectory(path); err != nil {
		t.Fatal(err)
	}
}
