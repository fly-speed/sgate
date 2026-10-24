package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestPasswordEncryptionBindsHandshake(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)
	password := "秘密 password\r\nwith spaces"
	id, nonce, ts, resume, uri := randomID(), randomID(), strconv.FormatInt(time.Now().Unix(), 10), "0", "/tunnel?x=1"
	token, e := encryptPassword(key, password, id, nonce, ts, resume, uri)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(token, password) || strings.ContainsAny(token, "\r\n") {
		t.Fatal("password was exposed in header")
	}
	if !verifyPassword(key, password, token, id, nonce, ts, resume, uri) {
		t.Fatal("correct password could not be decrypted")
	}
	next, e := encryptPassword(key, password, id, nonce, ts, resume, uri)
	if e != nil {
		t.Fatal(e)
	}
	if token == next {
		t.Fatal("password encryption reused its random nonce")
	}
	for _, tc := range []struct{ name, password, token, id, nonce, ts, resume, uri string }{
		{"wrong password", "wrong", token, id, nonce, ts, resume, uri},
		{"missing password header", password, "", id, nonce, ts, resume, uri},
		{"plaintext password", password, password, id, nonce, ts, resume, uri},
		{"bad encoding", password, "v1.!!!", id, nonce, ts, resume, uri},
		{"truncated", password, token[:len(token)-8], id, nonce, ts, resume, uri},
		{"tampered", password, token[:10] + "!" + token[11:], id, nonce, ts, resume, uri},
		{"different session", password, token, randomID(), nonce, ts, resume, uri},
		{"different nonce", password, token, id, randomID(), ts, resume, uri},
		{"different timestamp", password, token, id, nonce, "1", resume, uri},
		{"different resume flag", password, token, id, nonce, ts, "1", uri},
		{"different URI", password, token, id, nonce, ts, resume, "/other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if verifyPassword(key, tc.password, tc.token, tc.id, tc.nonce, tc.ts, tc.resume, tc.uri) {
				t.Fatal("invalid credential accepted")
			}
		})
	}
	if verifyPassword(bytes.Repeat([]byte{4}, 32), password, token, id, nonce, ts, resume, uri) {
		t.Fatal("wrong sgate.key decrypted password")
	}
}

func TestPasswordIsRequiredAndBounded(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	for _, p := range []string{"", strings.Repeat("x", maxPasswordBytes+1)} {
		if _, e := NewClient(ClientConfig{Key: key, Password: p, URL: "ws://127.0.0.1/tunnel"}); e == nil {
			t.Fatal("invalid client password accepted")
		}
		if s, e := NewServer(ServerConfig{Key: key, Password: p, Target: "127.0.0.1:1"}); e == nil {
			s.Close()
			t.Fatal("invalid server password accepted")
		}
	}
	p := strings.Repeat("x", maxPasswordBytes)
	token, e := encryptPassword(key, p, "id", "nonce", "timestamp", "0", "/tunnel")
	if e != nil || !verifyPassword(key, p, token, "id", "nonce", "timestamp", "0", "/tunnel") {
		t.Fatal("maximum length password rejected")
	}
}

func TestPasswordAuthenticationBeforeUpgrade(t *testing.T) {
	for _, secure := range []bool{false, true} {
		name := "HTTP"
		if secure {
			name = "HTTPS"
		}
		t.Run(name, func(t *testing.T) {
			key := bytes.Repeat([]byte{12}, 32)
			s, e := NewServer(ServerConfig{Key: key, Password: testPassword, Target: "127.0.0.1:1", Options: testOptions()})
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			var mu sync.Mutex
			var last http.Header
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				last = r.Header.Clone()
				mu.Unlock()
				s.ServeHTTP(w, r)
			})
			var server *httptest.Server
			if secure {
				server = httptest.NewTLSServer(handler)
			} else {
				server = httptest.NewServer(handler)
			}
			defer server.Close()
			endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/tunnel"
			d := websocket.Dialer{HandshakeTimeout: time.Second}
			if secure {
				roots := x509.NewCertPool()
				roots.AddCert(server.Certificate())
				d.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			}
			fresh := func(password string) http.Header {
				h, e := requestHeaders(
					key,
					password,
					randomID(),
					randomID(),
					strconv.FormatInt(time.Now().Unix(), 10),
					"0",
					"/tunnel",
				)
				if e != nil {
					t.Fatal(e)
				}
				return h
			}
			resign := func(h http.Header) {
				h.Set(
					"X-Sgate-Auth",
					mac(
						key,
						protocol,
						"client",
						h.Get("X-Sgate-Session"),
						h.Get("X-Sgate-Nonce"),
						h.Get("X-Sgate-Time"),
						h.Get("X-Sgate-Resume"),
						"/tunnel",
						h.Get(passwordHeader),
					),
				)
			}
			cases := []struct {
				name   string
				change func(http.Header)
			}{
				{"wrong password", func(h http.Header) {
					bad := fresh("wrong-password")
					for k := range h {
						delete(h, k)
					}
					for k, v := range bad {
						h[k] = v
					}
				}},
				{"missing header", func(h http.Header) {
					h.Del(passwordHeader)
					resign(h)
				}},
				{"plaintext header", func(h http.Header) {
					h.Set(passwordHeader, testPassword)
					resign(h)
				}},
				{"damaged GCM tag", func(h http.Header) {
					token := h.Get(passwordHeader)
					raw, _ := base64.RawURLEncoding.DecodeString(token[3:])
					raw[len(raw)-1] ^= 1
					h.Set(passwordHeader, "v1."+base64.RawURLEncoding.EncodeToString(raw))
					resign(h)
				}},
				{"duplicated header", func(h http.Header) { h.Add(passwordHeader, h.Get(passwordHeader)) }},
				{
					"rebound nonce with valid MAC",
					func(h http.Header) {
						h.Set("X-Sgate-Nonce", randomID())
						resign(h)
					},
				},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					h := fresh(testPassword)
					tc.change(h)
					ws, r, e := d.Dial(endpoint, h)
					if e == nil {
						ws.Close()
						t.Fatal("invalid password upgraded")
					}
					if r == nil {
						t.Fatal(e)
					}
					r.Body.Close()
					if r.StatusCode != 404 {
						t.Fatalf("wrong status: %d", r.StatusCode)
					}
					s.mu.Lock()
					clean := len(s.sessions) == 0 && len(s.nonces) == 0
					s.mu.Unlock()
					if !clean {
						t.Fatal("password rejection allocated session or replay cache")
					}
				})
			}
			client, e := NewClient(ClientConfig{
				Key:       key,
				Password:  testPassword,
				URL:       endpoint,
				TLSConfig: d.TLSClientConfig,
				Options:   testOptions(),
			})
			if e != nil {
				t.Fatal(e)
			}
			ws, _, _, e := client.dial(context.Background(), randomID(), false)
			if e != nil {
				t.Fatal(e)
			}
			mu.Lock()
			sent := last.Clone()
			mu.Unlock()
			if token := sent.Get(passwordHeader); token == "" || strings.Contains(token, testPassword) {
				t.Fatal("client did not encrypt password header")
			}
			ws.Close()
			// A previously valid complete password-bearing request must not be replayable.
			replay := http.Header{}
			for k, values := range sent {
				if strings.HasPrefix(k, "X-Sgate-") {
					replay[k] = values
				}
			}
			again, r, e := d.Dial(endpoint, replay)
			if e == nil {
				again.Close()
				t.Fatal("replayed encrypted password request accepted")
			}
			if r == nil {
				t.Fatal(e)
			}
			r.Body.Close()
			if r.StatusCode != 404 {
				t.Fatalf("replay status=%d", r.StatusCode)
			}
			badClient, e := NewClient(ClientConfig{
				Key:       key,
				Password:  "wrong-password",
				URL:       endpoint,
				TLSConfig: d.TLSClientConfig,
				Options:   testOptions(),
			})
			if e != nil {
				t.Fatal(e)
			}
			if conn, _, status, e := badClient.dial(context.Background(), randomID(), false); e == nil {
				conn.Close()
				t.Fatal("client with wrong password authenticated")
			} else if status != 404 {
				t.Fatalf("wrong password status=%d", status)
			}
		})
	}
}
