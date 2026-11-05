//go:build darwin || linux || windows

package vpn

import (
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
)

func TestClientRecoveryWaitsForOwnerAndKeepsFailedCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.vpn-recovery")
	r, err := openClientRecovery(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.lock.Close()
	endpoint := netip.MustParseAddr("192.0.2.10")
	if err := r.rememberRoute(endpoint, netip.MustParseAddr("192.0.2.1"), "physical", 1); err != nil {
		t.Fatal(err)
	}
	if !ClientRecoveryPending(path) {
		t.Fatal("pending route was not persisted")
	}
	called := false
	dns := func(clientRecoveryState) error { t.Fatal("unexpected DNS recovery"); return nil }
	route := func(s clientRecoveryState) error {
		called = true
		if s.Endpoint != endpoint {
			t.Fatal("unexpected recovery target", s.Endpoint)
		}
		return errors.New("test cleanup failed")
	}
	if err := recoverClientNetwork(path, dns, route); err == nil || called {
		t.Fatal("recovery ran while the current owner held the lock", err)
	}
	if err := r.lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recoverClientNetwork(path, dns, route); err == nil || !called || !ClientRecoveryPending(path) {
		t.Fatal("failed recovery did not preserve the record", err)
	}
	if err := recoverClientNetwork(path, dns, func(clientRecoveryState) error { return nil }); err != nil || ClientRecoveryPending(path) {
		t.Fatal("successful recovery did not clear the record", err)
	}
}
