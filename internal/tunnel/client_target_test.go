package tunnel

import (
	"bytes"
	"net/url"
	"testing"
)

func TestClientBackendSelectionURL(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, address, want string }{
		{"default", "ws://localhost/tunnel?x=one%20two", "", ""},
		{"legacy", "wss://localhost/tunnel?addr=localhost%3A8088&x=1", "", "localhost:8088"},
		{"override", "ws://localhost/tunnel?addr=127.0.0.1:1&x=1", "backend.example.com:443", "backend.example.com:443"},
		{"ipv6", "wss://localhost/tunnel", "[::1]:8088", "[::1]:8088"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewClient(ClientConfig{
				Key: bytes.Repeat([]byte{8}, 32), Password: testPassword,
				URL: tc.endpoint, Addr: tc.address,
			})
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(client.cfg.URL)
			if err != nil {
				t.Fatal(err)
			}
			if u.Query().Get("addr") != tc.want {
				t.Fatal("wrong selected backend", client.cfg.URL)
			}
			if tc.address == "" && client.cfg.URL != tc.endpoint {
				t.Fatal("legacy URL changed")
			}
			if original, _ := url.Parse(tc.endpoint); original.Query().Get("x") != u.Query().Get("x") {
				t.Fatal("unrelated query changed")
			}
		})
	}
}

func TestClientRejectsInvalidBackendBeforeDial(t *testing.T) {
	for _, tc := range []struct{ endpoint, address string }{
		{"ws://localhost/tunnel?addr=", ""},
		{"ws://localhost/tunnel?addr=host:80&addr=other:81", ""},
		{"ws://localhost/tunnel?addr=%zz", ""},
		{"ws://localhost/tunnel", "host:0"},
		{"ws://localhost/tunnel", "http://host:80"},
		{"ws://localhost/tunnel", "host:80\n"},
	} {
		_, err := NewClient(ClientConfig{
			Key: bytes.Repeat([]byte{8}, 32), Password: testPassword,
			URL: tc.endpoint, Addr: tc.address,
		})
		if err == nil {
			t.Fatal("invalid backend accepted")
		}
	}
}

func TestClientBackendOmitAddr(t *testing.T) {
	for _, query := range []string{
		"x=one%20two&addr=localhost:80",
		"addr=localhost:80&addr=other:81&x=one%20two",
		"addr=&x=one%20two",
	} {
		cfg := ClientConfig{
			Key: bytes.Repeat([]byte{8}, 32), Password: testPassword,
			URL: "wss://localhost/tunnel?" + query, Addr: "unused:0", OmitAddr: true,
		}
		client, err := NewClient(cfg)
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(client.cfg.URL)
		if u.Query().Has("addr") || u.Query().Get("x") != "one two" {
			t.Fatal("disabled selection leaked addr or lost another query", client.cfg.URL)
		}
		if cfg.URL != "wss://localhost/tunnel?"+query || cfg.Addr != "unused:0" {
			t.Fatal("saved selection was changed")
		}
	}
}
