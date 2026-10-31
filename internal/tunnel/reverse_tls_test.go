package tunnel

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func reverseTestCertificate(t *testing.T, serial int64, dnsOnly ...bool) (tls.Certificate, *x509.Certificate, string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "reverse-test"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	if len(dnsOnly) > 0 && dnsOnly[0] {
		template.IPAddresses = nil
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	rawKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rawKey})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err = os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	return pair, cert, certPath, keyPath
}

func reverseTLSBackend(t *testing.T, pair tls.Certificate) string {
	t.Helper()
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	var conns sync.Map
	var workers sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			conns.Store(c, true)
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conns.Delete(c)
				defer c.Close()
				c.SetDeadline(time.Now().Add(10 * time.Second))
				c.Write([]byte("T"))
				io.Copy(c, c)
			}()
		}
	}()
	t.Cleanup(func() {
		l.Close()
		<-done
		conns.Range(func(key, value any) bool { key.(net.Conn).Close(); return true })
		workers.Wait()
	})
	return l.Addr().String()
}

func reverseTLSLink(t *testing.T, s *Server, web *httptest.Server, roots *x509.CertPool) *reverseLink {
	t.Helper()
	if web.TLS != nil {
		roots.AddCert(web.Certificate())
	}
	c, err := NewClient(ClientConfig{AllowReverse: true, Key: s.cfg.Key, Username: "alice", Password: testPassword, URL: "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel", TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, Options: testOptions()})
	if err != nil {
		t.Fatal(err)
	}
	link := &reverseLink{client: c, id: randomID(), local: newSession(context.Background(), "", c.cfg.Options)}
	link.attach(t, s, false)
	t.Cleanup(func() {
		link.local.close()
		link.wsClose()
		select {
		case <-link.done:
		case <-time.After(3 * time.Second):
			t.Error("TLS transport cleanup blocked")
		}
	})
	return link
}

func TestReverseTLSModesAndCertificateSelection(t *testing.T) {
	fallback, fallbackCert, _, _ := reverseTestCertificate(t, 1)
	_, independentCert, certPath, keyPath := reverseTestCertificate(t, 2)
	targetPair, targetCert, _, _ := reverseTestCertificate(t, 3)
	for _, wss := range []bool{false, true} {
		for _, inbound := range []bool{false, true} {
			for _, outbound := range []bool{false, true} {
				for _, separate := range []bool{false, true} {
					if separate && !inbound {
						continue
					}
					t.Run(fmt.Sprintf("wss=%t/listenTLS=%t/targetTLS=%t/separate=%t", wss, inbound, outbound, separate), func(t *testing.T) {
						target, marker := targetBackend(t, "P"), "P"
						if outbound {
							target, marker = reverseTLSBackend(t, targetPair), "T"
						}
						mapping := ReverseMapping{Listen: reverseAddress(t), Target: target, ListenTLS: inbound, TargetTLS: outbound}
						expected := fallbackCert
						if separate {
							mapping.ListenTLSCert, mapping.ListenTLSKey = certPath, keyPath
							expected = independentCert
						}
						s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{9}, 32), Accounts: []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{mapping}}}, Target: "localhost:22", Options: testOptions(), ReverseTLSConfig: &tls.Config{Certificates: []tls.Certificate{fallback}, MinVersion: tls.VersionTLS12}})
						if err != nil {
							t.Fatal(err)
						}
						defer s.Close()
						if err = s.StartReverse(); err != nil {
							t.Fatal(err)
						}
						var web *httptest.Server
						if wss {
							web = httptest.NewTLSServer(s)
						} else {
							web = httptest.NewServer(s)
						}
						defer web.Close()
						roots := x509.NewCertPool()
						roots.AddCert(targetCert)
						link := reverseTLSLink(t, s, web, roots)
						var conn net.Conn
						if inbound {
							trust := x509.NewCertPool()
							trust.AddCert(expected)
							secure, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", mapping.Listen, &tls.Config{RootCAs: trust, MinVersion: tls.VersionTLS12})
							if err != nil {
								t.Fatal(err)
							}
							if !bytes.Equal(secure.ConnectionState().PeerCertificates[0].Raw, expected.Raw) {
								t.Fatal("wrong listener certificate")
							}
							conn = secure
						} else {
							conn = dialReverse(t, mapping.Listen)
						}
						defer conn.Close()
						conn.SetDeadline(time.Now().Add(5 * time.Second))
						readMarker(t, conn, marker)
						if err = exchange(conn, bytes.Repeat([]byte("TLS and TCP binary\x00\xff"), 2048)); err != nil {
							t.Fatal(err)
						}
						// A transport outage retains the TLS endpoints and replays on the same stream.
						link.wsClose()
						<-link.done
						waitFor(t, "server detached", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.sessions[link.id].inUse })
						link.attach(t, s, true)
						if err = exchange(conn, []byte("after reconnect")); err != nil {
							t.Fatal(err)
						}
						if err = conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
							t.Fatal(err)
						}
						var b [1]byte
						if _, err = conn.Read(b[:]); err != io.EOF {
							t.Fatal("missing half-close EOF", err)
						}
					})
				}
			}
		}
	}
}

func TestReverseTLSRejectsInvalidCertificatesAndLegacyClient(t *testing.T) {
	pair, _, certPath, keyPath := reverseTestCertificate(t, 5)
	mapping := ReverseMapping{Listen: reverseAddress(t), Target: "localhost:22", ListenTLS: true}
	base := ServerConfig{Key: bytes.Repeat([]byte{9}, 32), Target: "localhost:22", Options: testOptions()}
	base.Accounts = []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{mapping}}}
	if s, err := NewServer(base); err == nil {
		s.Close()
		t.Fatal("missing fallback certificate accepted")
	}
	mapping.ListenTLSCert = certPath
	mapping.ListenTLSKey = "missing-private-key"
	base.Accounts[0].ReverseMappings[0] = mapping
	if s, err := NewServer(base); err == nil {
		s.Close()
		t.Fatal("invalid private key accepted")
	}
	mapping.ListenTLSCert, mapping.ListenTLSKey = certPath, keyPath
	base.Accounts[0].ReverseMappings[0] = mapping
	s, err := NewServer(base)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.StartReverse(); err != nil {
		t.Fatal(err)
	}
	previous := s.reverseTLS[mapping.Listen]
	next := append([]Account(nil), base.Accounts...)
	next[0].ReverseMappings = []ReverseMapping{{Listen: mapping.Listen, Target: mapping.Target, ListenTLS: true, ListenTLSCert: certPath, ListenTLSKey: "missing"}}
	if err = s.UpdateAccounts(next); err == nil {
		t.Fatal("bad TLS update accepted")
	}
	if s.reverseTLS[mapping.Listen] != previous {
		t.Fatal("failed update changed live TLS certificate")
	}
	// A peer lacking authenticated reverse-TLS capability must never receive a
	// TLS target as a plaintext OPEN. Simulate a retained older client session.
	target := reverseTLSBackend(t, pair)
	next[0].ReverseMappings = []ReverseMapping{{Listen: mapping.Listen, Target: target, TargetTLS: true}}
	if err = s.UpdateAccounts(next); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(s)
	defer web.Close()
	link := reverseTLSLink(t, s, web, x509.NewCertPool())
	s.mu.Lock()
	s.sessions[link.id].reverseTLS = false
	s.mu.Unlock()
	assertSocketClosed(t, dialReverse(t, mapping.Listen))
	link.wsClose()
	<-link.done
	waitFor(t, "legacy capability transport detached", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.sessions[link.id].inUse })
	ws, _, status, err := link.client.dial(context.Background(), link.id, true)
	if ws != nil {
		ws.Close()
	}
	if err == nil || status != 409 {
		t.Fatal("TLS capability changed across resume", status, err)
	}

}

func TestReverseTLSTargetRejectsUntrustedCertificate(t *testing.T) {
	pair, _, _, _ := reverseTestCertificate(t, 6)
	target := reverseTLSBackend(t, pair)
	mapping := ReverseMapping{Listen: reverseAddress(t), Target: target, TargetTLS: true}
	s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{9}, 32), Target: "localhost:22", Accounts: []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{mapping}}}, Options: testOptions()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.StartReverse(); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(s)
	defer web.Close()
	link := reverseTLSLink(t, s, web, x509.NewCertPool())
	assertSocketClosed(t, dialReverse(t, mapping.Listen))
	waitFor(t, "failed TLS stream released", func() bool { link.local.mu.Lock(); defer link.local.mu.Unlock(); return len(link.local.streams) == 0 })
	s.mu.Lock()
	ready := s.sessions[link.id].ready
	s.mu.Unlock()
	if !ready {
		t.Fatal("bad target certificate destroyed tunnel")
	}
}

func TestReverseTLSHandshakeTimeoutAndShutdown(t *testing.T) {
	for _, listenerTLS := range []bool{false, true} {
		t.Run(fmt.Sprint("listenerTLS=", listenerTLS), func(t *testing.T) {
			pair, _, _, _ := reverseTestCertificate(t, 9)
			stalled, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer stalled.Close()
			accepted := make(chan net.Conn, 1)
			go func() {
				c, err := stalled.Accept()
				if err == nil {
					accepted <- c
				}
			}()
			opts := testOptions()
			opts.DialTimeout = 120 * time.Millisecond
			mapping := ReverseMapping{Listen: reverseAddress(t), Target: stalled.Addr().String(), ListenTLS: listenerTLS, TargetTLS: !listenerTLS}
			s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{9}, 32), Target: "localhost:22", Accounts: []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{mapping}}}, Options: opts, ReverseTLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.StartReverse(); err != nil {
				t.Fatal(err)
			}
			web := httptest.NewServer(s)
			defer web.Close()
			client, err := NewClient(ClientConfig{Key: s.cfg.Key, Username: "alice", Password: testPassword, URL: "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel", AllowReverse: true, Options: opts})
			if err != nil {
				t.Fatal(err)
			}
			link := &reverseLink{client: client, id: randomID(), local: newSession(context.Background(), "", client.cfg.Options)}
			link.attach(t, s, false)
			defer func() { link.local.close(); link.wsClose(); <-link.done }()
			conn := dialReverse(t, mapping.Listen)
			var target net.Conn
			select {
			case target = <-accepted:
				defer target.Close()
			case <-time.After(time.Second):
				t.Fatal("target never accepted")
			}
			// Either the external SSL client or the target intentionally stays silent.
			assertSocketClosed(t, conn)
			waitFor(t, "TLS timed-out stream released", func() bool {
				s.mu.Lock()
				remote := s.sessions[link.id].session
				s.mu.Unlock()
				remote.mu.Lock()
				defer remote.mu.Unlock()
				return len(remote.streams) == 0
			})
			closed := make(chan struct{})
			go func() { s.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("TLS shutdown blocked")
			}
		})
	}
}

func TestReverseTLSCommandRoleAndFrameValidation(t *testing.T) {
	command := frame{op: opReverseOpenTLS, seq: 1, stream: reverseStreamBase + 1, data: []byte("localhost:443")}
	if _, err := decodeFrame(command.encode()); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []frame{{op: opReverseOpenTLS, seq: 1, stream: 1, data: []byte("localhost:443")}, {op: opReverseOpenTLS, seq: 1, stream: reverseStreamBase + 1}, {op: opReverseOpenTLS, seq: 1, stream: reverseStreamBase + 1, data: []byte("https://localhost:443")}} {
		if _, err := decodeFrame(bad.encode()); err == nil {
			t.Fatal("malformed TLS OPEN accepted")
		}
	}
	for _, target := range []string{"", "localhost:22"} {
		opts := testOptions()
		opts, _ = opts.defaults()
		local := newSession(context.Background(), target, opts)
		if err := local.apply(command); err == nil {
			local.close()
			t.Fatal("TLS OPEN bypassed role or consent")
		}
		local.close()
	}
}

func TestReverseTLSTargetVerifiesHostname(t *testing.T) {
	pair, cert, _, _ := reverseTestCertificate(t, 10, true)
	target := reverseTLSBackend(t, pair)
	mapping := ReverseMapping{Listen: reverseAddress(t), Target: target, TargetTLS: true}
	s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{9}, 32), Target: "localhost:22", Accounts: []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{mapping}}}, Options: testOptions()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.StartReverse(); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(s)
	defer web.Close()
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	reverseTLSLink(t, s, web, roots)
	// The CA is trusted, but the certificate has only localhost, not 127.0.0.1.
	assertSocketClosed(t, dialReverse(t, mapping.Listen))
}

func TestReverseTLSCloseInterruptsBlockedWrite(t *testing.T) {
	pair, cert, _, _ := reverseTestCertificate(t, 11)
	rawServer, rawClient := net.Pipe()
	defer rawClient.Close()
	observed := &reverseTLSObservedConn{Conn: rawServer, writes: make(chan struct{}, 16)}
	server := &reverseTLSConn{Conn: tls.Server(observed, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})}
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	client := tls.Client(rawClient, &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12})
	handshaken := make(chan error, 1)
	go func() { handshaken <- client.Handshake() }()
	if err := server.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-handshaken; err != nil {
		t.Fatal(err)
	}
	opts, _ := testOptions().defaults()
	session := newSession(context.Background(), "", opts)
	session.mu.Lock()
	session.streams[1] = &stream{conn: server}
	session.mu.Unlock()
	for len(observed.writes) > 0 {
		<-observed.writes
	}
	writer := make(chan error, 1)
	go func() { _, err := server.Write(bytes.Repeat([]byte("blocked TLS write"), 65536)); writer <- err }()
	select {
	case <-observed.writes:
	case <-time.After(time.Second):
		t.Fatal("TLS writer never reached the blocked socket")
	}
	// net.Pipe has no buffering and the TLS peer never reads application data.
	closed := make(chan struct{})
	go func() { session.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("session close waited for blocked TLS write")
	}
	select {
	case err := <-writer:
		if err == nil {
			t.Fatal("blocked writer unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("TLS writer retained after close")
	}
}

func TestReverseTLSConcurrentConnections(t *testing.T) {
	pair, cert, _, _ := reverseTestCertificate(t, 12)
	target := reverseTLSBackend(t, pair)
	mapping := ReverseMapping{Listen: reverseAddress(t), Target: target, ListenTLS: true, TargetTLS: true}
	s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{9}, 32), Target: "localhost:22", Accounts: []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{mapping}}}, Options: testOptions(), ReverseTLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.StartReverse(); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(s)
	defer web.Close()
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	reverseTLSLink(t, s, web, roots)
	const workers = 32
	results := make(chan error, workers)
	for worker := 0; worker < workers; worker++ {
		go func(id int) {
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", mapping.Listen, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
			if err != nil {
				results <- err
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			var marker [1]byte
			if _, err = io.ReadFull(conn, marker[:]); err == nil && marker[0] != 'T' {
				err = fmt.Errorf("wrong marker %q", marker)
			}
			if err == nil {
				err = exchange(conn, bytes.Repeat([]byte(fmt.Sprintf("worker=%03d TLS payload;", id)), 1024))
			}
			if err == nil {
				err = conn.CloseWrite()
			}
			results <- err
		}(worker)
	}
	for worker := 0; worker < workers; worker++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestReverseTLSLiveModeSwitchRetainsRawListener(t *testing.T) {
	pair, cert, _, _ := reverseTestCertificate(t, 13)
	plainTarget := targetBackend(t, "P")
	secureTarget := reverseTLSBackend(t, pair)
	mapping := ReverseMapping{Listen: reverseAddress(t), Target: plainTarget}
	accounts := []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{mapping}}}
	s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{9}, 32), Target: "localhost:22", Accounts: accounts, Options: testOptions(), ReverseTLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.StartReverse(); err != nil {
		t.Fatal(err)
	}
	listener := s.reverseListeners[mapping.Listen]
	web := httptest.NewServer(s)
	defer web.Close()
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	reverseTLSLink(t, s, web, roots)
	old := dialReverse(t, mapping.Listen)
	readMarker(t, old, "P")
	mapping.ListenTLS, mapping.TargetTLS, mapping.Target = true, true, secureTarget
	accounts[0].ReverseMappings = []ReverseMapping{mapping}
	if err = s.UpdateAccounts(accounts); err != nil {
		t.Fatal(err)
	}
	assertSocketClosed(t, old)
	if s.reverseListeners[mapping.Listen] != listener {
		t.Fatal("TLS enable rebound the inherited/raw listener")
	}
	reverseTLSLink(t, s, web, roots)
	secure, err := tls.Dial("tcp", mapping.Listen, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer secure.Close()
	secure.SetDeadline(time.Now().Add(time.Second))
	readMarker(t, secure, "T")
	mapping.ListenTLS, mapping.TargetTLS, mapping.Target = false, false, plainTarget
	accounts[0].ReverseMappings = []ReverseMapping{mapping}
	if err = s.UpdateAccounts(accounts); err != nil {
		t.Fatal(err)
	}
	assertSocketClosed(t, secure)
	if s.reverseListeners[mapping.Listen] != listener {
		t.Fatal("TLS disable rebound the listener")
	}
	reverseTLSLink(t, s, web, roots)
	fresh := dialReverse(t, mapping.Listen)
	readMarker(t, fresh, "P")
}

type reverseTLSObservedConn struct {
	net.Conn
	writes chan struct{}
}

func (c *reverseTLSObservedConn) Write(p []byte) (int, error) {
	select {
	case c.writes <- struct{}{}:
	default:
	}
	return c.Conn.Write(p)
}
