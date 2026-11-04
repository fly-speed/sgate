package vpn

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/url"
)

func setServerURL(p *Prepared, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "quic" && u.Scheme != "tcp" && u.Scheme != "wss" && u.Scheme != "udp") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/vpn") {
		return invalidFields("需要 udp://、quic://、tcp:// 或 wss://主机:端口/vpn 地址。", "vpn-server")
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || host == "" {
		return invalidFields("VPN 地址必须包含服务器主机名和有效端口。", "vpn-server")
	}
	p.Host, p.Port = host, port
	p.Config.Address = u.Host
	p.Config.Transport = u.Scheme
	p.Config.Host = u.Host
	p.Config.TLS.ServerName = host
	return nil
}
func endpointFingerprint(base, address string) string {
	sum := sha256.Sum256([]byte(base + "\x00" + address))
	return hex.EncodeToString(sum[:])
}

// WithServerURL retains all applied credentials and network settings. Only the
// endpoint and TLS server name change; preparing a candidate performs no I/O.
func (p *Prepared) WithServerURL(address string) (*Prepared, error) {
	next := *p
	next.Config = p.Config
	next.Config.TLS = p.Config.TLS.Clone()
	if err := setServerURL(&next, address); err != nil {
		return nil, err
	}
	if err := ValidateConfig(next.Config, false); err != nil {
		return nil, err
	}
	next.Fingerprint = endpointFingerprint(next.baseFingerprint, address)
	return &next, nil
}
