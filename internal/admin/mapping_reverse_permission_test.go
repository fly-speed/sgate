package admin

import (
	"strings"
	"testing"
)

func TestMappingReversePermissionSurvivesMaskingAndCredentialEdits(t *testing.T) {
	raw := `[{"listen":"127.0.0.1:9001","server":"ws://localhost/tunnel","username":"alice","password":"private-secret","allow_reverse":true},{"listen":"127.0.0.1:9002","server":"ws://localhost/tunnel"}]`
	previous, err := proxyMappings(raw)
	if err != nil {
		t.Fatal(err)
	}
	view := maskedMappings(previous)
	if !view[0].AllowReverse || view[1].AllowReverse {
		t.Fatal("public mapping representation lost explicit consent")
	}
	restored, err := restoreMappingPasswords(view, previous)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeProxyMappings(restored)
	if err != nil {
		t.Fatal(err)
	}
	next, err := proxyMappings(encoded)
	if err != nil || !next[0].AllowReverse || next[1].AllowReverse {
		t.Fatal("permission changed during credential preservation", err)
	}
	next[0].AllowReverse = false
	encoded, err = encodeProxyMappings(next)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, `"allow_reverse":true`) {
		t.Fatal("revocation was not persisted")
	}
	if _, ok := argumentOptions("client")["allow-reverse"]; !ok {
		t.Fatal("single-proxy permission missing from client option rules")
	}
	if _, ok := argumentOptions("server")["allow-reverse"]; ok {
		t.Fatal("client permission leaked to server options")
	}
}
