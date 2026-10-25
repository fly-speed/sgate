package tunnel

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestReceiveWindowProofAndLegacy(t *testing.T) {
	key := bytes.Repeat([]byte{41}, 32)
	for _, kind := range []string{"new", "legacy", "tampered", "stripped", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nonce := randomID()
				window := "16409"
				if kind == "legacy" {
					window = ""
				}
				headers := http.Header{}
				headers.Set("X-Sgate-Nonce", nonce)
				headers.Set("X-Sgate-Auth", windowMAC(key, window, protocol, "server", r.Header.Get("X-Sgate-Session"), r.Header.Get("X-Sgate-Nonce"), nonce, r.Header.Get("X-Sgate-Time"), r.Header.Get(passwordHeader)))
				switch kind {
				case "tampered":
					window = "32818"
				case "stripped":
					window = ""
				case "invalid":
					window = "1"
				}
				if window != "" {
					headers.Set(receiveWindowHeader, window)
				}
				ws, err := (&websocket.Upgrader{}).Upgrade(w, r, headers)
				if err == nil {
					ws.Close()
				}
			}))
			defer h.Close()
			client, err := NewClient(ClientConfig{Key: key, Password: testPassword, URL: "ws" + strings.TrimPrefix(h.URL, "http") + "/tunnel"})
			if err != nil {
				t.Fatal(err)
			}
			ws, crypt, _, err := client.dial(context.Background(), randomID(), false)
			if ws != nil {
				defer ws.Close()
			}
			if kind == "new" || kind == "legacy" {
				if err != nil {
					t.Fatal(err)
				}
				want := 16409
				if kind == "legacy" {
					want = 0
				}
				if crypt.peerWindow != want {
					t.Fatal("wrong window", crypt.peerWindow)
				}
			} else if err == nil {
				t.Fatal("accepted unauthenticated/invalid window")
			}
		})
	}
}

func TestReceiveWindowRequestAuthentication(t *testing.T) {
	key := bytes.Repeat([]byte{41}, 32)
	server, err := NewServer(ServerConfig{Key: key, Password: testPassword, Target: "127.0.0.1:1", Options: testOptions()})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	for _, value := range []string{"16409", "1", "-1", "invalid", "999999999999999999999999"} {
		uri := "/tunnel?sgate-window=" + value
		headers, err := requestHeaders(key, testPassword, randomID(), randomID(), strconv.FormatInt(time.Now().Unix(), 10), "0", uri)
		if err != nil {
			t.Fatal(err)
		}
		headers.Set("Upgrade", "websocket")
		headers.Set("Connection", "upgrade")
		r := httptest.NewRequest("GET", uri, nil)
		r.Header = headers
		_, ok := server.authenticateRequest(r)
		if ok != (value == "16409") {
			t.Fatalf("window %q accepted=%t", value, ok)
		}
		if ok {
			r.URL.RawQuery = "sgate-window=32818"
			if _, ok := server.authenticateRequest(r); ok {
				t.Fatal("tampered request window accepted")
			}
		}
	}
}
