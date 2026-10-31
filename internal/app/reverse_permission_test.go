package app

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"sgate/internal/admin"
	"sgate/internal/tunnel"
)

func TestClientReversePermissionConfigIsIndependentAndDefaultsOff(t *testing.T) {
	routes := []clientProxy{{Listen: "127.0.0.1:19001", Server: "ws://localhost:8080/tunnel"}, {Listen: "127.0.0.1:19002", Server: "ws://localhost:8080/tunnel", AllowReverse: true}, {Listen: "127.0.0.1:19003", Server: "ws://localhost:8080/tunnel"}}
	data, _ := json.Marshal(routes)
	c := proxyConfigController(t, "proxies = '"+string(data)+"'\nusername = alice\nallow-reverse = true\n")
	d, err := c.load("")
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{false, true, false} {
		if d.proxies[i].config.AllowReverse != want {
			t.Fatalf("mapping %d inherited another permission", i)
		}
	}
	for _, test := range []struct {
		content string
		args    []string
		want    bool
	}{
		{content: ""}, {content: "allow-reverse = true\n", want: true}, {content: "allow-reverse = true\n", args: []string{"-allow-reverse=false"}},
	} {
		single := proxyConfigController(t, "server = ws://localhost:8080/tunnel\nusername = alice\n"+test.content, test.args...)
		next, err := single.load("")
		if err != nil || next.client.AllowReverse != test.want {
			t.Fatal("single-proxy permission/CLI mismatch", err)
		}
	}
}

func TestClientReversePermissionLiveRevocationIsolatedPerMapping(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%t", secure), func(t *testing.T) {
			forward, reverse := onlineBackend(t, "F"), onlineBackend(t, "R")
			text := "target = " + forward + "\nmax-sessions = 3\n"
			scheme, clientTLS := "ws", ""
			if secure {
				cert, key := managedCertificate(t)
				text += "tls-cert = " + cert + "\ntls-key = " + key + "\n"
				scheme = "wss"
				clientTLS = "tls-ca = " + cert + "\n"
			}
			server := newController(t, "server", text)
			snapshot, err := server.manageTunnelAccounts(nil)
			if err != nil {
				t.Fatal(err)
			}
			exposed := onlineAddress(t)
			mappings := []tunnel.ReverseMapping{{Listen: exposed, Target: reverse}}
			if _, err = server.manageTunnelAccounts(&admin.TunnelAccountChange{Action: "create", Revision: snapshot.Revision, Username: "alice", Password: "multi-test-password", ReverseMappings: &mappings}); err != nil {
				t.Fatal(err)
			}
			first, second := onlineAddress(t), onlineAddress(t)
			endpoint := scheme + "://" + server.worker.listener.Addr().String() + "/tunnel"
			routes := []clientProxy{{Listen: first, Server: endpoint}, {Listen: second, Server: endpoint}}
			raw, _ := json.Marshal(routes)
			// Even a true single-proxy switch does not grant permission to any JSON row.
			client := startOnlineController(t, "username = alice\nallow-reverse = true\nproxies = '"+string(raw)+"'\n"+clientTLS, false, "-key-file", filepath.Join(filepath.Dir(server.path), "key"))
			keep := onlineConnect(t, first, "F")
			oldSecond := onlineConnect(t, second, "F")
			deny := func() {
				t.Helper()
				c, err := net.DialTimeout("tcp", exposed, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				c.SetReadDeadline(time.Now().Add(time.Second))
				var p [1]byte
				n, err := c.Read(p[:])
				if n != 0 || err == nil {
					t.Fatal("reverse data reached a mapping without permission")
				}
				if e, ok := err.(net.Error); ok && e.Timeout() {
					t.Fatal("disabled reverse admission hung")
				}
			}
			deny()
			group := client.worker.proxies
			group.mu.RLock()
			originalFirst := group.routes[client.current.proxies[0].port]
			originalSecond := group.routes[client.current.proxies[1].port]
			group.mu.RUnlock()
			routes[1].AllowReverse = true
			setOnlineMappings(t, client, routes)
			if err = client.applyLive([]string{"proxies"}); err != nil {
				t.Fatal(err)
			}
			group.mu.RLock()
			enabledSecond := group.routes[client.current.proxies[1].port]
			sameFirst := group.routes[client.current.proxies[0].port] == originalFirst
			group.mu.RUnlock()
			if !sameFirst || enabledSecond == originalSecond {
				t.Fatal("permission change did not isolate worker replacement")
			}
			select {
			case <-originalSecond.done:
			case <-time.After(time.Second):
				t.Fatal("old unprivileged worker survived")
			}
			oldSecond.SetReadDeadline(time.Now().Add(time.Second))
			if n, _ := oldSecond.Read(make([]byte, 1)); n != 0 {
				t.Fatal("replaced worker delivered stale data")
			}
			onlineExchange(t, keep)
			existingReverse := reverseAppConnect(t, exposed, "R")
			onlineExchange(t, existingReverse)
			routes[1].AllowReverse = false
			setOnlineMappings(t, client, routes)
			if err = client.applyLive([]string{"proxies"}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-enabledSecond.done:
			case <-time.After(time.Second):
				t.Fatal("privileged worker survived revocation")
			}
			existingReverse.SetReadDeadline(time.Now().Add(time.Second))
			if n, err := existingReverse.Read(make([]byte, 1)); n != 0 || err == nil {
				t.Fatal("existing reverse socket survived revocation")
			} else if e, ok := err.(net.Error); ok && e.Timeout() {
				t.Fatal("reverse socket revocation timed out")
			}
			deny()
			onlineExchange(t, keep)
			onlineExchange(t, onlineConnect(t, second, "F"))
			// With only three server slots, retaining deliberately stopped
			// unprivileged sessions would exhaust admission on the next grant.
			for cycle := 0; cycle < 3; cycle++ {
				routes[1].AllowReverse = true
				setOnlineMappings(t, client, routes)
				if err = client.applyLive([]string{"proxies"}); err != nil {
					t.Fatal(err)
				}
				active := reverseAppConnect(t, exposed, "R")
				routes[1].AllowReverse = false
				setOnlineMappings(t, client, routes)
				if err = client.applyLive([]string{"proxies"}); err != nil {
					t.Fatal(err)
				}
				active.SetReadDeadline(time.Now().Add(time.Second))
				if n, err := active.Read(make([]byte, 1)); n != 0 || err == nil {
					t.Fatal("repeated revocation retained socket")
				} else if e, ok := err.(net.Error); ok && e.Timeout() {
					t.Fatal("repeated revocation timed out")
				}
				active.Close()
				onlineExchange(t, keep)
			}
			group.mu.RLock()
			sameFirst = group.routes[client.current.proxies[0].port] == originalFirst
			group.mu.RUnlock()
			if !sameFirst {
				t.Fatal("revocation restarted unrelated mapping")
			}
		})
	}
}

func TestClientReversePermissionSingleFallbackDoesNotInheritOrApplyPendingEdits(t *testing.T) {
	for _, test := range []struct {
		name            string
		global, mapping bool
		pending         string
		want            bool
	}{
		{name: "no inheritance from enabled mapping", mapping: true},
		{name: "pending grant stays pending", pending: "true"},
		{name: "explicit single mode setting", global: true, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			address := onlineAddress(t)
			routes := []clientProxy{{Listen: address, Server: "ws://127.0.0.1:1/tunnel", AllowReverse: test.mapping}}
			raw, _ := json.Marshal(routes)
			client := startOnlineController(t, "listen = "+address+"\nserver = ws://127.0.0.1:1/tunnel\nallow-reverse = "+fmt.Sprint(test.global)+"\nproxies = '"+string(raw)+"'\n", false)
			if test.pending != "" {
				setConfig(t, client, "allow-reverse", test.pending)
			}
			setOnlineMappings(t, client, nil)
			if err := client.applyLive([]string{"proxies"}); err != nil {
				t.Fatal(err)
			}
			if client.current.client == nil || client.current.client.AllowReverse != test.want {
				t.Fatal("single fallback inherited a mapping grant or applied a pending security edit")
			}
		})
	}
}
