package app

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestAllowedTargetsConfigApplyAndCLIOverride(t *testing.T) {
	for _, override := range []bool{false, true} {
		name := "file"
		var args []string
		if override {
			name = "cli"
			args = []string{"-allowed-targets", "localhost:8088"}
		}
		t.Run(name, func(t *testing.T) {
			c := newController(t, "server", "target = 127.0.0.1:9001\nallowed-targets = 127.0.0.1:8088, example.com:443\n", args...)
			want := []string{"127.0.0.1:8088", "example.com:443"}
			if override {
				want = []string{"localhost:8088"}
			}
			if !reflect.DeepEqual(c.current.server.AllowedTargets, want) {
				t.Fatal(c.current.server.AllowedTargets)
			}
			setConfig(t, c, "allowed-targets", "127.0.0.1:8089")
			if err := c.apply(); err != nil {
				t.Fatal(err)
			}
			if !override {
				want = []string{"127.0.0.1:8089"}
			}
			if !reflect.DeepEqual(c.current.server.AllowedTargets, want) {
				t.Fatal("allowlist apply lost CLI priority")
			}
			if !override {
				worker := c.worker
				setConfig(t, c, "allowed-targets", "127.0.0.1:0")
				if err := c.apply(); err == nil || c.worker != worker {
					t.Fatal("invalid allowlist replaced a running proxy")
				}
			}
		})
	}
}

func TestClientMappingBackendConfigApplyAndCLIOverride(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprint(override), func(t *testing.T) {
			listen := temporaryAddress(t)
			mapping := func(addr string) string {
				raw, _ := json.Marshal([]clientProxy{{Listen: listen, Server: "ws://127.0.0.1:1/tunnel?addr=localhost:80", Addr: addr}})
				return string(raw)
			}
			var args []string
			if override {
				args = []string{"-proxies", mapping("localhost:8089")}
			}
			c := newController(t, "client", "proxies = '"+mapping("localhost:8088")+"'\n", args...)
			want := "localhost:8088"
			if override {
				want = "localhost:8089"
			}
			if c.current.proxies[0].config.Addr != want {
				t.Fatal("mapping backend priority")
			}
			setConfig(t, c, "proxies", mapping("localhost:8090"))
			if err := c.apply(); err != nil {
				t.Fatal(err)
			}
			if !override {
				want = "localhost:8090"
			}
			if c.current.proxies[0].config.Addr != want {
				t.Fatal("mapping override lost")
			}
			if !override {
				old := c.worker
				setConfig(t, c, "proxies", mapping("localhost:0"))
				if err := c.apply(); err == nil || c.worker != old {
					t.Fatal("invalid mapping replaced worker")
				}
			}
		})
	}
}

func TestClientSendAddrConfigApplyAndCLIOverride(t *testing.T) {
	for _, override := range []string{"file", "true", "false"} {
		t.Run(override, func(t *testing.T) {
			var args []string
			if override != "file" {
				args = []string{"-send-addr=" + override}
			}
			c := newController(t, "client", testClientMapping(t, "ws://127.0.0.1:1/tunnel?addr=localhost:80")+"addr = localhost:81\nsend-addr = false\n", args...)
			if c.current.client.OmitAddr != (override != "true") {
				t.Fatal("send-addr priority mismatch")
			}
			for _, enabled := range []string{"true", "false", "true"} {
				setConfig(t, c, "send-addr", enabled)
				if err := c.apply(); err != nil {
					t.Fatal(err)
				}
				want := enabled == "false"
				if override != "file" {
					want = override == "false"
				}
				if c.current.client.OmitAddr != want || c.current.client.Addr != "localhost:81" {
					t.Fatal("toggle lost override or saved addr")
				}
			}
			if override == "file" {
				worker := c.worker
				setConfig(t, c, "send-addr", "invalid")
				if err := c.apply(); err == nil || c.worker != worker {
					t.Fatal("invalid boolean replaced running client")
				}
			}
		})
	}
}

func TestAllowedTargetsOnlineKeepsWorkerAndPendingConfig(t *testing.T) {
	for _, override := range []bool{false, true} {
		name := "file"
		var args []string
		if override {
			name = "cli"
			args = []string{"-allowed-targets", "localhost:8088"}
		}
		t.Run(name, func(t *testing.T) {
			c := newController(t, "server", "target = 127.0.0.1:9001\nallowed-targets = localhost:8088\n", args...)
			worker := c.worker
			setConfig(t, c, "target", "127.0.0.1:9999")
			for _, value := range []string{"localhost:8089", "", "localhost:8090,localhost:8091"} {
				setConfig(t, c, "allowed-targets", value)
				if err := c.applyLive([]string{"allowed-targets"}); err != nil {
					t.Fatal(err)
				}
				if c.worker != worker || c.generation != 1 || c.current.server.Target != "127.0.0.1:9001" {
					t.Fatal("online update restarted worker or applied pending target")
				}
				next, err := c.load("")
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(c.current.server.AllowedTargets, next.server.AllowedTargets) {
					t.Fatal("online policy lost CLI priority")
				}
			}
			if !override {
				before := append([]string(nil), c.current.server.AllowedTargets...)
				setConfig(t, c, "allowed-targets", "localhost:0")
				if err := c.applyLive([]string{"allowed-targets"}); err == nil || !reflect.DeepEqual(c.current.server.AllowedTargets, before) {
					t.Fatal("invalid policy replaced runtime")
				}
			}
		})
	}
}
