package vpn

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func integratedSettings(t *testing.T) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("vpn", flag.ContinueOnError)
	RegisterFlags(fs, "client")
	key := filepath.Join(t.TempDir(), "key")
	if e := os.WriteFile(key, []byte(strings.Repeat("01", 32)), 0600); e != nil {
		t.Fatal(e)
	}
	fs.String("key-file", key, "")
	fs.String("tls-ca", "", "")
	fs.Set("vpn-server", "quic://vpn.invalid:8444/vpn")
	return fs
}
func TestIntegratedVPNPrepareIsOfflineAndValidatesBeforeTUN(t *testing.T) {
	fs := integratedSettings(t)
	p, e := Prepare("client", fs)
	if e != nil || p != nil {
		t.Fatal("disabled changed behavior", p, e)
	}
	fs.Set("vpn-mode", "only")
	p, e = Prepare("client", fs)
	if e != nil {
		t.Fatal("must not resolve .invalid hostname", e)
	}
	if p.Mode != "only" || p.Config.TLS.ServerName != "vpn.invalid" || !p.Manage || p.Config.MTU != 1280 {
		t.Fatal(p)
	}
	first := p.Fingerprint
	fs.Set("vpn-mtu", "1200")
	p, e = Prepare("client", fs)
	if e != nil || first == p.Fingerprint {
		t.Fatal("change not detected", e)
	}
	for _, change := range [][2]string{{"vpn-server", "ws://example.com:443/vpn"}, {"vpn-server", "quic://user:password@example.com:443/vpn"}, {"vpn-server", "quic://example.com:0/vpn"}, {"vpn-network", "10.77.0.2/24"}, {"vpn-mtu", "1501"}, {"vpn-dns", "::1"}, {"vpn-routes", "::/0"}} {
		f := integratedSettings(t)
		f.Set("vpn-mode", "both")
		f.Set(change[0], change[1])
		if _, e := Prepare("client", f); e == nil {
			t.Fatal("invalid settings accepted", change[0])
		}
	}
}

func TestIntegratedVPNQuotedEmptyKeyFileUsesSharedKey(t *testing.T) {
	fs := integratedSettings(t)
	fs.Set("vpn-mode", "only")
	fs.Set("vpn-key-file", "''")
	if _, err := Prepare("client", fs); err != nil {
		t.Fatal(err)
	}
}

func TestIntegratedVPNCertificateVerificationOptIn(t *testing.T) {
	fs := integratedSettings(t)
	fs.Set("vpn-mode", "only")
	for _, transport := range []string{"quic", "tcp", "wss", "udp"} {
		t.Run(transport, func(t *testing.T) {
			fs.Set("vpn-server", transport+"://vpn.invalid:8444/vpn")
			fs.Set("vpn-tls-ca", "")
			fs.Set("vpn-tls-skip-verify", "false")
			strict, err := Prepare("client", fs)
			if err != nil {
				t.Fatal(err)
			}
			config, err := strict.Config.normalized(false)
			if err != nil || config.TLS.InsecureSkipVerify {
				t.Fatal("certificate verification was disabled by default", err)
			}
			fs.Set("vpn-tls-ca", filepath.Join(t.TempDir(), "missing.pem"))
			if _, err := Prepare("client", fs); transport != "udp" && err == nil {
				t.Fatal("strict TLS accepted a missing CA")
			}
			fs.Set("vpn-tls-skip-verify", "true")
			skip, err := Prepare("client", fs)
			if err != nil {
				t.Fatal(err)
			}
			config, err = skip.Config.normalized(false)
			if err != nil || config.TLS.InsecureSkipVerify != (transport != "udp") || skip.Fingerprint == strict.Fingerprint {
				t.Fatal("explicit opt-in failed or affected UDP", err)
			}
		})
	}
}

func TestVPNNoiseSettingsAndLimits(t *testing.T) {
	fs := integratedSettings(t)
	fs.Set("vpn-mode", "both")
	fs.Set("vpn-noise-rate", "32K")
	p, e := Prepare("client", fs)
	if e != nil {
		t.Fatal(e)
	}
	if p.Config.Noise.Rate != 32768 || !p.Config.Noise.Traffic || !p.Config.Noise.Idle {
		t.Fatal(p.Config.Noise)
	}
	fs.Set("vpn-noise-rate", "11M")
	if _, e = Prepare("client", fs); e == nil {
		t.Fatal("unbounded noise rate")
	}
	fs.Set("vpn-noise-rate", "1K")
	fs.Set("vpn-noise-idle-after", "-1s")
	if _, e = Prepare("client", fs); e == nil {
		t.Fatal("negative idle threshold accepted")
	}
}

func TestVPNQueueExpiryIsOptInForOldConfigurations(t *testing.T) {
	fs := integratedSettings(t)
	fs.Set("vpn-mode", "only")
	p, err := Prepare("client", fs)
	if err != nil || p.Config.QueueMaxAge != 0 {
		t.Fatal("missing setting enabled age expiry", p, err)
	}
	before := p.Fingerprint
	fs.Set("vpn-queue-max-age", "250ms")
	p, err = Prepare("client", fs)
	if err != nil || p.Config.QueueMaxAge.String() != "250ms" || p.Fingerprint != before {
		t.Fatal("live queue age changed static VPN identity", p, err)
	}
	for _, value := range []string{"-1ms", "61s"} {
		fs.Set("vpn-queue-max-age", value)
		if _, err := Prepare("client", fs); err == nil {
			t.Fatal("invalid queue age accepted", value)
		}
	}
}

func TestIntegratedVPNTransportSelectionAndFingerprint(t *testing.T) {
	fs := integratedSettings(t)
	fs.Set("vpn-mode", "only")
	var previous string
	for _, scheme := range []string{"quic", "tcp", "wss"} {
		fs.Set("vpn-server", scheme+"://vpn.invalid:443/vpn")
		p, err := Prepare("client", fs)
		if err != nil {
			t.Fatal(scheme, err)
		}
		if p.Config.Transport != scheme || p.Config.Host != "vpn.invalid:443" || p.Config.TLS.ServerName != "vpn.invalid" || p.Fingerprint == previous {
			t.Fatal(scheme, p)
		}
		previous = p.Fingerprint
	}
}

func TestVPNServerCandidateMatchesFreshFingerprintWithoutChangingAppliedNetwork(t *testing.T) {
	fs := integratedSettings(t)
	fs.Set("vpn-mode", "both")
	applied, err := Prepare("client", fs)
	if err != nil {
		t.Fatal(err)
	}
	address := "wss://vpn-next.invalid:443/vpn"
	candidate, err := applied.WithServerURL(address)
	if err != nil {
		t.Fatal(err)
	}
	fs.Set("vpn-server", address)
	fresh, err := Prepare("client", fs)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Fingerprint != fresh.Fingerprint || candidate.Config.TLS.ServerName != "vpn-next.invalid" || applied.Config.TLS.ServerName != "vpn.invalid" || candidate.Network.MTU != applied.Network.MTU {
		t.Fatal("endpoint candidate changed unrelated settings or identity")
	}
}
