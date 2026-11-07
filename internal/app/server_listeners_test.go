package app

import (
	"testing"

	"sgate/internal/tunnel"
)

func TestServerHTTPListenersExcludeReverseTCP(t *testing.T) {
	accounts := []tunnel.Account{{ReverseMappings: []tunnel.ReverseMapping{{Listen: "127.0.0.1:10022"}}}}
	got, err := serverHTTPListeners("|1080, [::1]|8081, 127.0.0.1|10022", accounts)
	if err != nil || got != ":1080,[::1]:8081" {
		t.Fatal(got, err)
	}
	if _, err = serverHTTPListeners("127.0.0.1|10022", accounts); err == nil {
		t.Fatal("accepted a configuration without HTTP listeners")
	}
}

func TestServerUsesSharedListenerWithoutLegacyEntry(t *testing.T) {
	c := newController(t, "server", "master_service = 127.0.0.1|0\ntarget = localhost:22\n")
	if c.current.listen != "127.0.0.1:0" {
		t.Fatal(c.current.listen)
	}
}
