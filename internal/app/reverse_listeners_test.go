package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mastergin "github.com/acl-dev/gin-service"
	"github.com/gin-gonic/gin"

	"sgate/internal/admin"
	"sgate/internal/tunnel"
)

type reverseManagedHTTP struct {
	ctx       context.Context
	addresses []string
}

func (s reverseManagedHTTP) Configure(HTTPConfig) (HTTPWorker, error) {
	return nil, errors.New("unused")
}
func (s reverseManagedHTTP) Context() context.Context { return s.ctx }
func (s reverseManagedHTTP) Err() error               { return nil }
func (s reverseManagedHTTP) Addresses() []string      { return s.addresses }
func (s reverseManagedHTTP) Managed() bool            { return true }

func TestReverseManagedSaveSynchronizesBothFilesAndDefersActivation(t *testing.T) {
	for _, withScripts := range []bool{true, false} {
		t.Run(map[bool]string{true: "success", false: "rollback"}[withScripts], func(t *testing.T) {
			c := newController(t, "server", "listen = "+onlineAddress(t)+"\ntarget = localhost:22\n")
			address := c.worker.listener.Addr().String()
			listener := strings.ReplaceAll(address, ":", "|")
			setConfig(t, c, "master_service", listener)
			content, _ := os.ReadFile(c.path)
			if err := os.WriteFile(c.path, append(append([]byte("service server {\n"), content...), []byte("\n}\n")...), 0600); err != nil {
				t.Fatal(err)
			}
			c.servicePath, c.serviceListen = c.path, listener
			c.service = reverseManagedHTTP{ctx: c.ctx, addresses: []string{address}}
			if withScripts {
				for _, name := range []string{"sgate-server-stop.sh", "sgate-server-start.sh"} {
					if err := os.WriteFile(filepath.Join(filepath.Dir(c.path), name), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			api := liveAccountManagement(t, c)
			before, _ := os.ReadFile(c.path)
			snapshot := apiObject(t, api.call("GET", "/api/tunnel-accounts", nil))
			mappings := []tunnel.ReverseMapping{{Listen: onlineAddress(t), Target: "localhost:22"}}
			response := api.call("PUT", "/api/tunnel-accounts", map[string]any{"action": "create", "username": "alice", "password": "secret", "reverse_mappings": mappings, "revision": snapshot["revision"]})
			after, _ := os.ReadFile(c.path)
			if !withScripts {
				if response.Code == 200 || string(before) != string(after) {
					t.Fatal("failed setup did not restore config", response.Body.String(), string(after))
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(c.path), "tunnel-accounts.json")); !os.IsNotExist(err) {
					t.Fatal("failed setup retained accounts", err)
				}
				return
			}
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			result := apiObject(t, response)
			if result["service_restart_required"] != true || result["requires_restart"] != true || result["listeners_changed"] != true || result["pending"] != true {
				t.Fatal(result)
			}
			if !strings.Contains(string(after), strings.ReplaceAll(mappings[0].Listen, ":", "|")) {
				t.Fatal("missing framework port", string(after))
			}
			if len(c.current.server.Accounts) != 0 {
				t.Fatal("pending accounts activated before listener restart")
			}
			saved, err := tunnel.LoadAccounts(c.current.accountsFile)
			if err != nil || len(saved) != 1 || len(saved[0].ReverseMappings) != 1 {
				t.Fatal(saved, err)
			}
			settings, err := processSettings(c.path)
			if err != nil {
				t.Fatal(err)
			}
			if c.pendingListeners == "" || sameClientAddresses(settings["master_service"], c.serviceListen) {
				t.Fatal("missing restart intent")
			}
			// Another target-only save while a port restart is pending must retain intent.
			mappings[0].Target = "localhost:80"
			updated := api.call("PUT", "/api/tunnel-accounts", map[string]any{"action": "update", "username": "alice", "reverse_mappings": mappings, "revision": result["revision"]})
			if updated.Code != 200 || apiObject(t, updated)["service_restart_required"] != true {
				t.Fatal(updated.Body.String())
			}
			// Deleting the pending account restores the original listener set immediately.
			result = apiObject(t, updated)
			deleted := api.call("PUT", "/api/tunnel-accounts", map[string]any{"action": "delete", "username": "alice", "revision": result["revision"]})
			if deleted.Code != 200 || apiObject(t, deleted)["service_restart_required"] == true || c.pendingListeners != "" {
				t.Fatal(deleted.Body.String())
			}
			settings, _ = processSettings(c.path)
			if !sameClientAddresses(settings["master_service"], listener) {
				t.Fatal(settings)
			}
		})
	}
}

type reverseLeaseHTTP struct{ *mastergin.GinService }

func (s reverseLeaseHTTP) Managed() bool { return true }
func (s reverseLeaseHTTP) Configure(c HTTPConfig) (HTTPWorker, error) {
	return s.GinService.Configure(mastergin.ServerConfig{Addresses: c.Addresses, TLSConfig: c.TLS, Handler: c.Handler, WrapListener: c.WrapListener, OnStop: c.OnStop})
}
func (s reverseLeaseHTTP) ReverseListener(_ string, address string) (net.Listener, error) {
	return s.LeaseTCP(address)
}

func TestReverseInheritedFrameworkSocketEndToEnd(t *testing.T) {
	backend := onlineBackend(t, "R")
	c := newController(t, "server", "listen = "+onlineAddress(t)+"\ntarget = "+backend+"\n")
	httpAddress := c.worker.listener.Addr().String()
	snapshot, err := c.manageTunnelAccounts(nil)
	if err != nil {
		t.Fatal(err)
	}
	rawAddress := onlineAddress(t)
	mappings := []tunnel.ReverseMapping{{Listen: rawAddress, Target: backend}}
	if _, err = c.manageTunnelAccounts(&admin.TunnelAccountChange{Action: "create", Revision: snapshot.Revision, Username: "alice", Password: "secret", ReverseMappings: &mappings}); err != nil {
		t.Fatal(err)
	}
	if err = c.worker.stop(); err != nil {
		t.Fatal(err)
	}
	c.worker = nil
	// Acquire real sockets once, then exercise the managed lifecycle without
	// launching acl_master: Configure must lease, never bind those ports again.
	framework, err := mastergin.Init(httpAddress+","+rawAddress, mastergin.WithContext(c.ctx), mastergin.WithManaged(false))
	if err != nil {
		t.Fatal(err)
	}
	defer framework.Close()
	framework.Alone = false
	framework.GET("/*path", gin.WrapH(framework.Handler()))
	service := reverseLeaseHTTP{framework}
	d := c.current
	d.service = service
	d.shared = &sharedServer{}
	c.shared = d.shared
	c.current = d
	c.service = service
	c.worker, err = d.start(c.ctx, c.stats)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { framework.Run(); close(done) }()
	defer func() { framework.Close(); <-done }()
	newController(t, "client", "server = ws://"+httpAddress+"/tunnel\nusername = alice\nallow-reverse = true\nreconnect-min = 10ms\nreconnect-max = 20ms\n", "-password", "secret")
	conn := reverseAppConnect(t, rawAddress, "R")
	onlineExchange(t, conn)
	// Shared HTTP replacement transfers the same raw lease before closing the
	// prior protocol server, so inherited sockets never need a second bind.
	for generation := 0; generation < 3; generation++ {
		if err := c.replaceShared(c.current); err != nil {
			t.Fatal(err)
		}
		conn = reverseAppConnect(t, rawAddress, "R")
		onlineExchange(t, conn)
	}
	// Ordinary HTTP stays on the tunnel port; the raw port delivered binary TCP.
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + httpAddress + "/missing")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func TestReverseTunnelPortConflictLeavesFilesUnchanged(t *testing.T) {
	c := newController(t, "server", "listen = "+onlineAddress(t)+"\ntarget = localhost:22\n")
	_, port, _ := net.SplitHostPort(c.worker.listener.Addr().String())
	// A different bind IP could otherwise succeed at the OS level.
	conflict := []tunnel.ReverseMapping{{Listen: "127.0.0.2:" + port, Target: "localhost:22"}}
	snapshot, err := c.manageTunnelAccounts(nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(c.path)
	if _, err = c.manageTunnelAccounts(&admin.TunnelAccountChange{Action: "create", Revision: snapshot.Revision, Username: "alice", Password: "secret", ReverseMappings: &conflict}); !errors.Is(err, admin.ErrReverseTunnelPortConflict) {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(c.path)
	if string(after) != string(before) {
		t.Fatal("conflict changed service configuration")
	}
	if _, err = os.Stat(snapshot.Path); !os.IsNotExist(err) {
		t.Fatal("conflict created account file", err)
	}
	// Existing accounts are protected as well, without touching JSON or runtime.
	snapshot, err = c.manageTunnelAccounts(&admin.TunnelAccountChange{Action: "create", Revision: snapshot.Revision, Username: "alice", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ = os.ReadFile(c.path)
	accountsBefore, _ := os.ReadFile(snapshot.Path)
	if _, err = c.manageTunnelAccounts(&admin.TunnelAccountChange{Action: "update", Revision: snapshot.Revision, Username: "alice", ReverseMappings: &conflict}); !errors.Is(err, admin.ErrReverseTunnelPortConflict) {
		t.Fatal(err)
	}
	after, _ = os.ReadFile(c.path)
	accountsAfter, _ := os.ReadFile(snapshot.Path)
	if string(after) != string(before) || string(accountsBefore) != string(accountsAfter) || c.pendingListeners != "" || c.restartPending {
		t.Fatal("conflict changed files or restart state")
	}
	// Startup/file loading also rejects conflicts against the effective CLI port.
	c.args = append(c.args, "-listen", "127.0.0.1:1080")
	data, err := tunnel.EncodeAccounts([]tunnel.Account{{Username: "alice", Password: "secret", ReverseMappings: []tunnel.ReverseMapping{{Listen: "[::1]:1080", Target: "localhost:22"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(snapshot.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = c.load(""); !errors.Is(err, admin.ErrReverseTunnelPortConflict) {
		t.Fatal("CLI listen override conflict accepted", err)
	}
}
