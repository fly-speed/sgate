package vpn

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"net"
	"net/netip"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"sgate/internal/cli"
)

// Setting describes one Web-editable integrated VPN option. Missing options in
// older service files are offered by the UI and added only when explicitly saved.
type Setting struct{ Key, Default, Description string }

func Settings(role string) []Setting {
	s := []Setting{
		{"vpn-mode", "disabled", "独立 VPN：disabled 关闭，both 与代理共存，only 仅 VPN（保留框架及管理监听）。应用时会中断 VPN 并恢复/重建网络。"},
		{"vpn-network", "10.77.0.0/24", "两端一致的 IPv4 VPN 子网，避免与现有网络重叠。"},
		{"vpn-mtu", "1280", "TUN MTU，576～1500 字节。"},
		{"vpn-tun-name", "", "新建设备名；Linux 默认 sgate0，macOS 自动分配 utunN。"},
		{"vpn-manage-network", "true", "自动配置路由、DNS 或服务端 NAT；需要管理员权限。"},
		{"vpn-key-file", "", "VPN 共享密钥文件；留空复用 key-file；不是代理或 Web 账号认证。"},
	}
	if role == "server" {
		return append(s,
			Setting{"vpn-listen", "0.0.0.0:8444", "独立 QUIC UDP 监听，不能与 quic-listen 共用 UDP 端口。"},
			Setting{"vpn-tls-cert", "", "VPN 服务器证书；留空复用 tls-cert。"},
			Setting{"vpn-tls-key", "", "VPN 证书私钥；留空复用 tls-key。"},
			Setting{"vpn-max-clients", "64", "最多同时认证客户端数，1～254；各客户端须配置不同虚拟 IP。"},
			Setting{"vpn-egress-interface", "", "Linux 物理出口网卡；留空按当前路由识别。"},
			Setting{"vpn-allow-private", "false", "允许访问服务器侧私网；始终禁止客户端互访。"})
	}
	return append(s,
		Setting{"vpn-server", "", "VPN 服务端 quic://证书域名:8444/vpn；独立于代理 server。"},
		Setting{"vpn-client-ip", "10.77.0.2", "该客户端固定虚拟 IPv4 地址，不能与其他客户端重复。"},
		Setting{"vpn-tls-ca", "", "附加信任 CA；留空复用 tls-ca。自签名证书需要显式信任。"},
		Setting{"vpn-routes", "0.0.0.0/1,128.0.0.0/1", "逗号分隔 IPv4 路由；服务端外层地址保留物理出口。"},
		Setting{"vpn-dns", "1.1.1.1", "VPN IPv4 DNS，逗号分隔；留空不改 DNS。"},
		Setting{"vpn-dns-service", "Wi-Fi", "macOS 网络服务名称；Linux 使用 resolvectl。"},
		Setting{"vpn-block-ipv6", "true", "首版不转发 IPv6；将公网 IPv6 导入 TUN 后丢弃，避免绕过 VPN。"})
}
func RegisterFlags(fs *flag.FlagSet, role string) {
	for _, s := range Settings(role) {
		switch s.Key {
		case "vpn-manage-network", "vpn-allow-private", "vpn-block-ipv6":
			fs.Bool(s.Key, s.Default == "true", s.Description)
		case "vpn-mtu":
			fs.Int(s.Key, 1280, s.Description)
		case "vpn-max-clients":
			fs.Int(s.Key, 64, s.Description)
		default:
			fs.String(s.Key, s.Default, s.Description)
		}
	}
}

type Prepared struct {
	Config                       Config
	Network                      NetworkOptions
	Role, Mode, TUN, Fingerprint string
	Manage                       bool
	Host, Port                   string
}

// Prepare validates settings without opening devices, issuing privileged commands
// or resolving remote hosts. It is safe for Web candidate validation.
func Prepare(role string, fs *flag.FlagSet) (*Prepared, error) {
	get := func(k string) string {
		if f := fs.Lookup(k); f != nil {
			return f.Value.String()
		}
		return ""
	}
	mode := get("vpn-mode")
	if mode == "" || mode == "disabled" {
		return nil, nil
	}
	if mode != "both" && mode != "only" {
		return nil, errors.New("vpn-mode must be disabled, both or only")
	}
	p := &Prepared{Role: role, Mode: mode, TUN: get("vpn-tun-name"), Manage: get("vpn-manage-network") == "true"}
	if role == "server" && runtime.GOOS != "linux" {
		return nil, errors.New("VPN server requires Linux")
	}
	keyPath := get("vpn-key-file")
	if keyPath == "" {
		keyPath = get("key-file")
	}
	key, err := cli.LoadKey(keyPath)
	if err != nil {
		return nil, err
	}
	network, err := netip.ParsePrefix(get("vpn-network"))
	if err != nil {
		return nil, err
	}
	return prepareValues(p, fs, key, network, get)
}
func prepareValues(p *Prepared, fs *flag.FlagSet, key []byte, network netip.Prefix, get func(string) string) (*Prepared, error) {
	mtu := fs.Lookup("vpn-mtu").Value.(flag.Getter).Get().(int)
	p.Config = Config{Key: key, Network: network, MTU: mtu, ClientIP: netip.MustParseAddr("10.77.0.2"), Address: "127.0.0.1:8444", MaxClients: 64}
	p.Network = NetworkOptions{Server: p.Role == "server", Network: network, MTU: mtu, Egress: get("vpn-egress-interface"), BlockIPv6: get("vpn-block-ipv6") == "true", DNSService: get("vpn-dns-service")}
	p.Config.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	if p.Role == "server" {
		certPath, keyPath := get("vpn-tls-cert"), get("vpn-tls-key")
		if certPath == "" {
			certPath = get("tls-cert")
		}
		if keyPath == "" {
			keyPath = get("tls-key")
		}
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, err
		}
		p.Config.TLS.Certificates = []tls.Certificate{cert}
		p.Config.Address = get("vpn-listen")
		p.Config.MaxClients = fs.Lookup("vpn-max-clients").Value.(flag.Getter).Get().(int)
		p.Config.AllowPrivate = get("vpn-allow-private") == "true"
		if q := get("quic-listen"); q != "" {
			_, a, e1 := net.SplitHostPort(q)
			_, b, e2 := net.SplitHostPort(p.Config.Address)
			if e1 == nil && e2 == nil && a == b {
				return nil, errors.New("vpn-listen and quic-listen require different UDP ports")
			}
		}
	} else {
		u, err := url.Parse(get("vpn-server"))
		if err != nil || u.Scheme != "quic" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/vpn") {
			return nil, errors.New("vpn-server must be quic://hostname:port/vpn")
		}
		host, port, err := net.SplitHostPort(u.Host)
		if err != nil || host == "" {
			return nil, errors.New("vpn-server requires hostname and UDP port")
		}
		p.Host, p.Port = host, port
		p.Config.Address = u.Host
		p.Config.TLS.ServerName = host
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		ca := get("vpn-tls-ca")
		if ca == "" {
			ca = get("tls-ca")
		}
		if ca != "" {
			b, e := os.ReadFile(ca)
			if e != nil {
				return nil, e
			}
			if !roots.AppendCertsFromPEM(b) {
				return nil, errors.New("VPN CA contains no certificates")
			}
		}
		p.Config.TLS.RootCAs = roots
		ip, err := netip.ParseAddr(get("vpn-client-ip"))
		if err != nil {
			return nil, err
		}
		p.Config.ClientIP = ip
		p.Network.ClientIP = ip
		if raw := get("vpn-routes"); raw != "" {
			for _, r := range strings.Split(raw, ",") {
				prefix, e := netip.ParsePrefix(strings.TrimSpace(r))
				if e != nil || !prefix.Addr().Is4() {
					return nil, errors.New("vpn-routes requires IPv4 prefixes")
				}
				if prefix.Bits() == 0 {
					p.Network.Routes = append(p.Network.Routes, netip.MustParsePrefix("0.0.0.0/1"), netip.MustParsePrefix("128.0.0.0/1"))
				} else {
					p.Network.Routes = append(p.Network.Routes, prefix.Masked())
				}
			}
		}
		if raw := get("vpn-dns"); raw != "" {
			for _, r := range strings.Split(raw, ",") {
				ip, e := netip.ParseAddr(strings.TrimSpace(r))
				if e != nil || !ip.Is4() {
					return nil, errors.New("vpn-dns requires IPv4 addresses")
				}
				p.Network.DNS = append(p.Network.DNS, ip)
			}
		}
	}
	if err := ValidateConfig(p.Config, p.Role == "server"); err != nil {
		return nil, err
	}
	// Include certificate material: reapplying after replacing a PEM also restarts VPN.
	values := map[string]string{}
	for _, s := range Settings(p.Role) {
		values[s.Key] = get(s.Key)
	}
	for _, k := range []string{"key-file", "tls-cert", "tls-key", "tls-ca"} {
		values[k] = get(k)
	}
	b, _ := json.Marshal(values)
	h := sha256.New()
	h.Write(b)
	h.Write(key)
	for _, cert := range p.Config.TLS.Certificates {
		for _, der := range cert.Certificate {
			h.Write(der)
		}
	}
	if p.Config.TLS.RootCAs != nil {
		for _, der := range p.Config.TLS.RootCAs.Subjects() {
			h.Write(der)
		}
	}
	p.Fingerprint = hex.EncodeToString(h.Sum(nil))
	return p, nil
}
func (p *Prepared) Resolve(ctx context.Context) (Config, NetworkOptions, error) {
	c, n := p.Config, p.Network
	if p.Role == "client" {
		resolveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ips, err := net.DefaultResolver.LookupNetIP(resolveCtx, "ip4", p.Host)
		if err != nil {
			return c, n, err
		}
		if len(ips) == 0 {
			return c, n, errors.New("VPN endpoint needs IPv4")
		}
		n.Endpoint = ips[0].Unmap()
		c.Address = net.JoinHostPort(n.Endpoint.String(), p.Port)
		for _, dns := range n.DNS {
			if dns == n.Endpoint {
				return c, n, errors.New("VPN DNS must differ from outer endpoint")
			}
		}
	}
	return c, n, nil
}
