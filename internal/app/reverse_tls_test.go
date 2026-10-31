package app

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"strings"
	"testing"
	"time"

	"sgate/internal/admin"
	"sgate/internal/tunnel"
)

func TestReverseTLSManagedCertificateFailurePreservesBothFiles(t *testing.T) {
	c := newController(t, "server", "listen = "+onlineAddress(t)+"\ntarget = localhost:22\n")
	snapshot, err := c.manageTunnelAccounts(nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = c.manageTunnelAccounts(&admin.TunnelAccountChange{Action: "create", Revision: snapshot.Revision, Username: "alice", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	listener := c.worker.listener.Addr().String()
	setConfig(t, c, "master_service", strings.ReplaceAll(listener, ":", "|"))
	c.service = reverseManagedHTTP{ctx: c.ctx, addresses: []string{listener}}
	c.serviceListen = strings.ReplaceAll(listener, ":", "|")
	c.servicePath = c.path
	before, _ := os.ReadFile(c.path)
	accountsBefore, _ := os.ReadFile(snapshot.Path)
	mappings := []tunnel.ReverseMapping{{Listen: onlineAddress(t), Target: "localhost:22", ListenTLS: true, ListenTLSCert: "/missing/cert.pem", ListenTLSKey: "/missing/key.pem"}}
	if _, err = c.manageTunnelAccounts(&admin.TunnelAccountChange{Action: "update", Revision: snapshot.Revision, Username: "alice", ReverseMappings: &mappings}); err == nil {
		t.Fatal("unreadable TLS files accepted before managed restart")
	}
	after, _ := os.ReadFile(c.path)
	accountsAfter, _ := os.ReadFile(snapshot.Path)
	if string(before) != string(after) || string(accountsBefore) != string(accountsAfter) || c.pendingListeners != "" {
		t.Fatal("certificate failure changed files or restart intent")
	}
}

func TestReverseTLSReusesActualTunnelCertificate(t *testing.T) {
	cert, key := managedCertificate(t)
	backend := onlineBackend(t, "R")
	c := newController(t, "server", "listen = "+onlineAddress(t)+"\ntarget = "+backend+"\ntls-cert = "+cert+"\ntls-key = "+key+"\n")
	snapshot, err := c.manageTunnelAccounts(nil)
	if err != nil {
		t.Fatal(err)
	}
	mapping := tunnel.ReverseMapping{Listen: onlineAddress(t), Target: backend, ListenTLS: true}
	mappings := []tunnel.ReverseMapping{mapping}
	if _, err = c.manageTunnelAccounts(&admin.TunnelAccountChange{Action: "create", Revision: snapshot.Revision, Username: "alice", Password: "secret", ReverseMappings: &mappings}); err != nil {
		t.Fatal(err)
	}
	newController(t, "client", "server = wss://"+c.worker.listener.Addr().String()+"/tunnel\nusername = alice\nallow-reverse = true\ntls-ca = "+cert+"\nreconnect-min = 10ms\nreconnect-max = 20ms\n", "-password", "secret")
	pemData, err := os.ReadFile(cert)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemData) {
		t.Fatal("invalid test CA")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := tls.Dial("tcp", mapping.Listen, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots})
		if err == nil {
			conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
			var marker [1]byte
			_, err = conn.Read(marker[:])
			if err == nil && marker[0] == 'R' {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(time.Second))
				onlineExchange(t, conn)
				return
			}
			conn.Close()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("reverse TLS listener never served using the tunnel certificate")
}

func TestReverseTLSManagedRestartValidatesSavedTunnelCertificate(t *testing.T) {
	cert, key := managedCertificate(t)
	c := newController(t, "server", "listen = "+onlineAddress(t)+"\ntarget = localhost:22\ntls-cert = "+cert+"\ntls-key = "+key+"\n")
	snapshot, err := c.manageTunnelAccounts(nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = c.manageTunnelAccounts(&admin.TunnelAccountChange{Action: "create", Revision: snapshot.Revision, Username: "alice", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	listener := c.worker.listener.Addr().String()
	setConfig(t, c, "master_service", strings.ReplaceAll(listener, ":", "|"))
	// The current process still has a certificate; the next process would not.
	setConfig(t, c, "tls-cert", "")
	setConfig(t, c, "tls-key", "")
	content, _ := os.ReadFile(c.path)
	content = append(append([]byte("service server {\n"), content...), []byte("\n}\n")...)
	if err = os.WriteFile(c.path, content, 0600); err != nil {
		t.Fatal(err)
	}
	c.service = reverseManagedHTTP{ctx: c.ctx, addresses: []string{listener}}
	c.serviceListen = strings.ReplaceAll(listener, ":", "|")
	c.servicePath = c.path
	accountsBefore, _ := os.ReadFile(snapshot.Path)
	mappings := []tunnel.ReverseMapping{{Listen: onlineAddress(t), Target: "localhost:22", ListenTLS: true}}
	_, err = c.manageTunnelAccounts(&admin.TunnelAccountChange{Action: "update", Revision: snapshot.Revision, Username: "alice", ReverseMappings: &mappings})
	if err == nil || !strings.Contains(err.Error(), "requires tunnel certificate") {
		t.Fatal("saved TLS configuration not checked", err)
	}
	after, _ := os.ReadFile(c.path)
	accountsAfter, _ := os.ReadFile(snapshot.Path)
	if string(after) != string(content) || string(accountsAfter) != string(accountsBefore) || c.pendingListeners != "" {
		t.Fatal("failed restart certificate preflight changed files")
	}
}
