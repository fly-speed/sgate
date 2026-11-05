package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitialSetupTokenFollowsManagementConfigAndAccount(t *testing.T) {
	token := strings.Repeat("0123456789abcdef", 4)
	for _, separate := range []bool{false, true} {
		dir := t.TempDir()
		c := New(dir)
		adminDir := filepath.Dir(c.Config)
		config := "service client {\nadmin_enabled = true\nadmin_users_file = custom.json\nadmin_setup_token_file = custom.token\n}\n"
		write := func(path, data string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if separate {
			adminDir = filepath.Join(adminDir, "admin")
			write(filepath.Join(adminDir, "admin.cf"), "users-file = custom.json\nsetup-token-file = custom.token\n")
			config = "service client {\nadmin-config = admin/admin.cf\n}\n"
		}
		write(c.Config, config)
		if c.InitialSetupToken() != "" {
			t.Fatal("showed token before it was generated")
		}
		tokenPath := filepath.Join(adminDir, "custom.token")
		write(tokenPath, "invalid")
		if c.InitialSetupToken() != "" {
			t.Fatal("showed malformed token")
		}
		write(tokenPath, token+"\r\n")
		if c.InitialSetupToken() != token {
			t.Fatalf("configured token was not found, separate=%v", separate)
		}
		write(filepath.Join(adminDir, "custom.json"), "{}")
		if c.InitialSetupToken() != "" {
			t.Fatal("showed leftover token when account file exists")
		}
	}
}
