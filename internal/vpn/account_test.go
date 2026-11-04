package vpn

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sgate/internal/cli"
)

func TestVPNAccountAuthenticationAcrossTransports(t *testing.T) {
	dir := t.TempDir()
	serverAccounts := filepath.Join(dir, "server-accounts.json")
	clientCredentials := filepath.Join(dir, "client-credentials.json")
	for path, data := range map[string]string{
		serverAccounts:    `{"version":1,"accounts":[{"username":"alice","password":"valid-password"},{"username":"disabled","password":"disabled-password","disabled":true}]}`,
		clientCredentials: `{"version":1,"accounts":[{"username":"alice","password":"valid-password"}]}`,
	} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, base, remote := fixtureWire(t, 4, NoiseConfig{}, "mixed", func(c *Config) {
		if err := c.LoadAccountFile(serverAccounts, true); err != nil {
			t.Fatal(err)
		}
	})
	legacy, reply := rawClient(t, base, base.ClientIP.String())
	legacy.CloseWithError(0, "test ended")
	if !strings.Contains(reply.Error, "username") || len(s.Clients()) != 0 {
		t.Fatal("server admitted a shared-key-only handshake")
	}
	for _, mode := range []string{"tcp", "wss", "udp", "quic"} {
		t.Run(mode, func(t *testing.T) {
			c := base
			// Exercise a client that has no local account requirement yet.
			c.RequireAccount = false
			c.Transport = mode
			switch mode {
			case "udp":
				c.Address = s.UDPAddr().String()
			case "tcp", "wss":
				c.Address = s.TCPAddr().String()
			}
			c.Host = c.Address
			for _, credentials := range [][2]string{{"", ""}, {"alice", "incorrect"}, {"disabled", "disabled-password"}} {
				c.Username, c.Password = credentials[0], credentials[1]
				result := Probe(context.Background(), c)
				if result.OK || result.Stage != "auth" || !strings.Contains(result.Error, "username") {
					t.Fatalf("unexpected failed-auth result: %+v", result)
				}
			}
			if len(s.Clients()) != 0 {
				t.Fatal("rejected credentials reserved an address")
			}
			if err := c.LoadAccountFile(clientCredentials, false); err != nil {
				t.Fatal(err)
			}
			if result := Probe(context.Background(), c); !result.OK {
				t.Fatalf("valid credentials: %+v", result)
			}
			c.Stats = &Stats{}
			local, stop, _ := startClient(t, c)
			await(t, "VPN account authenticated", func() bool { return c.Stats.Snapshot().Active == 1 })
			packet := ipPacket(c.ClientIP.String(), "8.8.8.8", 100)
			local.out <- packet
			receivePacket(t, remote, packet)
			stop()
			await(t, "account session removed", func() bool { return len(s.Clients()) == 0 })
		})
	}
}

func TestVPNClientCredentialsRelativePathAndFailClosed(t *testing.T) {
	fs := integratedSettings(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sgate-client.cf")
	credentials := filepath.Join(dir, "client.json")
	write := func(file, data string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(path, "vpn-mode = only\nvpn-credentials-file = client.json\n")
	write(credentials, `{"version":1,"accounts":[{"username":"alice","password":"first"}]}`)
	if err := cli.Parse(fs, []string{"-config", path}); err != nil {
		t.Fatal(err)
	}
	first, err := Prepare("client", fs)
	if err != nil || !first.Config.RequireAccount || first.Config.Username != "alice" || first.Config.Password != "first" {
		t.Fatal("client did not load its dedicated credentials", err)
	}
	if err := signHello(first.Config, assignmentChallengePrefix+"legacy", new(hello)); err == nil {
		t.Fatal("client downgraded to a shared-key-only handshake")
	}
	write(credentials, `{"version":1,"accounts":[{"username":"alice","password":"second"}]}`)
	second, err := Prepare("client", fs)
	if err != nil || first.Fingerprint == second.Fingerprint {
		t.Fatal("changed credentials did not invalidate the VPN definition", err)
	}
	write(credentials, `{"version":1,"accounts":[]}`)
	if _, err := Prepare("client", fs); err == nil {
		t.Fatal("empty client credentials were accepted")
	}
	if err := fs.Set("vpn-credentials-file", ""); err != nil {
		t.Fatal(err)
	}
	if err := fs.Set("vpn-require-account", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare("client", fs); err == nil {
		t.Fatal("required credentials fell back after removing the path")
	}
}
