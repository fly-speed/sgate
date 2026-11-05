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
	"os"
	"path/filepath"
	"slices"
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
		{"vpn-queue-max-age", "0s", "发送队列过期时间，0 关闭过期丢弃（默认）；最多 1m。弱网 TCP 建议保持 0，队列容量仍有上限。"},
		{"vpn-udp-send-interval", "0s", "纯 UDP VPN 每条连接发送大包的最小间隔，0 关闭，最多 10ms；仅影响本端发送，保存后立即生效。"},
		{"vpn-tun-name", "", "新建设备名；Linux 默认 sgate0，macOS 自动分配 utunN。"},
		{"vpn-manage-network", "true", "自动配置路由、DNS 或服务端 NAT；需要管理员权限。"},
		{"vpn-key-file", "", "VPN 共享密钥文件；留空复用 key-file；不是代理或 Web 账号认证。"},
		{"vpn-require-account", "false", "要求配置 VPN 专用账号文件并启用账号认证；配置文件路径后自动启用。"},
		{"vpn-noise-rate", "0", "本端每条 VPN 连接的白噪音负载字节/秒，支持 K/M，0 关闭，上限 10M；与代理 noise-* 参数独立。"},
		{"vpn-noise-traffic", "true", "业务 IP 数据传输期间允许白噪音；拥塞或有待发送业务时优先业务。"},
		{"vpn-noise-idle", "true", "VPN 已连接但无业务 IP 数据时允许白噪音；不要求存在业务连接。"},
		{"vpn-noise-idle-after", "1s", "连续多久没有双向业务 IP 数据视为空闲；0 使用 1s。"},
	}
	if role == "server" {
		return append(s,
			Setting{"vpn-accounts-file", "", "VPN 服务端专用账号 JSON 文件，相对路径按本配置目录解析；填写后启用账号认证。"},
			Setting{"vpn-listen", "0.0.0.0:8444", "独立 QUIC UDP 监听，不能与 quic-listen 共用 UDP 端口；配置 TCP 监听后可留空以关闭 UDP。"},
			Setting{"vpn-udp-listen", "", "纯 UDP VPN 监听，例如 0.0.0.0:8445；与 QUIC 分开 UDP 端口，业务包不确认或重传，使用共享密钥加密认证。"},
			Setting{"vpn-tcp-listen", "", "可选 TLS/TCP 与 WSS 共用监听，例如 0.0.0.0:8444；可与 VPN UDP 使用相同数字端口，不能与其他 TCP 服务共用。"},
			Setting{"vpn-tcp-notsent-lowat", "0", "Linux VPN 服务端 TCP/WSS 套接字的未发送数据水位，支持 K/M，0 使用系统默认，最大 1M；重启 VPN 生效。"},
			Setting{"vpn-tls-cert", "", "VPN 服务器证书；留空复用 tls-cert。"},
			Setting{"vpn-tls-key", "", "VPN 证书私钥；留空复用 tls-key。"},
			Setting{"vpn-max-clients", "64", "最多同时认证客户端数，1～254；降低上限不踢现有客户端，仅限制新连接；各客户端虚拟 IP 唯一。"},
			Setting{"vpn-egress-interface", "", "macOS/Linux 物理出口网卡；留空按当前路由识别。"},
			Setting{"vpn-allow-private", "false", "允许访问服务器侧私网；始终禁止客户端互访。"})
	}
	return append(s,
		Setting{"vpn-credentials-file", "", "VPN 客户端专用凭据 JSON 文件，包含一个账号；相对路径按本配置目录解析。"},
		Setting{"vpn-server", "", "VPN 服务端 udp://、quic://、tcp:// 或 wss://主机:端口/vpn；UDP 使用共享密钥加密，业务包不重传，其余方式使用 TLS。保存后自动重连，已停止的 VPN 不会自动启动。"},
		Setting{"vpn-tls-ca", "", "附加信任 CA；留空复用 tls-ca。自签名证书需要显式信任。"},
		Setting{"vpn-routes", "0.0.0.0/1,128.0.0.0/1", "逗号分隔 IPv4 路由；服务端外层地址保留物理出口。"},
		Setting{"vpn-dns", "1.1.1.1", "VPN IPv4 DNS，逗号分隔；留空不改 DNS。"},
		Setting{"vpn-dns-service", "Wi-Fi", "macOS 网络服务名称；Linux 使用 resolvectl。"},
		Setting{"vpn-block-ipv6", "true", "首版不转发 IPv6；将公网 IPv6 导入 TUN 后丢弃，避免绕过 VPN。"})
}

var legacyServerFileOptions = []string{
	"vpn-server", "vpn-client-ip", "vpn-tls-ca", "vpn-routes",
	"vpn-dns", "vpn-dns-service", "vpn-block-ipv6",
}

func IsLegacyServerFileOption(key string) bool {
	return slices.Contains(legacyServerFileOptions, key)
}

func RegisterFlags(fs *flag.FlagSet, role string) {
	if role == "client" {
		fs.String("vpn-client-ip", "", "deprecated; ignored, address is assigned by server")
	}
	if role == "server" {
		for _, key := range legacyServerFileOptions {
			cli.IgnoreFileOption(fs, key)
		}
	}
	for _, s := range Settings(role) {
		switch s.Key {
		case "vpn-require-account", "vpn-manage-network", "vpn-allow-private", "vpn-block-ipv6", "vpn-noise-traffic", "vpn-noise-idle":
			fs.Bool(s.Key, s.Default == "true", s.Description)
		case "vpn-noise-rate", "vpn-tcp-notsent-lowat":
			cli.ByteSizeVar(fs, new(int), s.Key, 0, s.Description)
		case "vpn-noise-idle-after":
			fs.Duration(s.Key, time.Second, s.Description)
		case "vpn-queue-max-age", "vpn-udp-send-interval":
			fs.Duration(s.Key, 0, s.Description)
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
	baseFingerprint              string
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
	if support := PlatformSupport(role); !support.Supported {
		return nil, invalidFields(support.Reason, "vpn-mode")
	}
	keyPath := get("vpn-key-file")
	if keyPath == "" {
		keyPath = get("key-file")
	}
	key, err := cli.LoadKey(keyPath)
	if err != nil {
		return nil, invalidFields("VPN 共享密钥无法加载："+err.Error(), "vpn-mode", "vpn-key-file")
	}
	network, err := netip.ParsePrefix(get("vpn-network"))
	if err != nil {
		return nil, err
	}
	return prepareValues(p, fs, key, network, get)
}
func prepareValues(p *Prepared, fs *flag.FlagSet, key []byte, network netip.Prefix, get func(string) string) (*Prepared, error) {
	mtu := fs.Lookup("vpn-mtu").Value.(flag.Getter).Get().(int)
	noiseRate, noiseErr := cli.ParseByteSize(get("vpn-noise-rate"))
	if noiseErr != nil {
		return nil, noiseErr
	}
	idleAfter, noiseErr := time.ParseDuration(get("vpn-noise-idle-after"))
	if noiseErr != nil {
		return nil, noiseErr
	}
	p.Config = Config{Key: key, Network: network, MTU: mtu, Address: "127.0.0.1:8444", MaxClients: 64}
	p.Network = NetworkOptions{Server: p.Role == "server", Network: network, MTU: mtu, Egress: get("vpn-egress-interface"), BlockIPv6: get("vpn-block-ipv6") == "true", DNSService: get("vpn-dns-service")}
	p.Config.QueueMaxAge, noiseErr = time.ParseDuration(get("vpn-queue-max-age"))
	if noiseErr != nil {
		return nil, noiseErr
	}
	p.Config.UDPSendInterval = fs.Lookup("vpn-udp-send-interval").Value.(flag.Getter).Get().(time.Duration)
	p.Config.Noise = NoiseConfig{Rate: noiseRate, Traffic: get("vpn-noise-traffic") == "true", Idle: get("vpn-noise-idle") == "true", IdleAfter: idleAfter}
	p.Config.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	p.Config.RequireAccount = get("vpn-require-account") == "true"
	accountField := "vpn-credentials-file"
	if p.Role == "server" {
		accountField = "vpn-accounts-file"
	}
	accountPath := get(accountField)
	if accountPath != "" && !filepath.IsAbs(accountPath) && get("config") != "" {
		accountPath = filepath.Join(filepath.Dir(get("config")), accountPath)
	}
	if err := p.Config.LoadAccountFile(accountPath, p.Role == "server"); err != nil {
		return nil, invalidFields("无法加载 VPN 专用账号文件："+err.Error(), accountField, "vpn-require-account")
	}

	if p.Role == "server" {
		p.Config.TCPNotSentLowat, noiseErr = cli.ParseByteSize(get("vpn-tcp-notsent-lowat"))
		if noiseErr != nil {
			return nil, invalidFields(noiseErr.Error(), "vpn-tcp-notsent-lowat")
		}
		p.Config.Address = get("vpn-listen")
		p.Config.TCPListen = get("vpn-tcp-listen")
		p.Config.UDPListen = get("vpn-udp-listen")
		if p.Config.Address != "" || p.Config.TCPListen != "" {
			certPath, keyPath := get("vpn-tls-cert"), get("vpn-tls-key")
			if certPath == "" {
				certPath = get("tls-cert")
			}
			if keyPath == "" {
				keyPath = get("tls-key")
			}
			if certPath == "" || keyPath == "" {
				return nil, invalidFields("启用 VPN 需要服务器证书和私钥：填写 vpn-tls-cert / vpn-tls-key，或配置可复用的 tls-cert / tls-key。", "vpn-mode", "vpn-tls-cert", "vpn-tls-key")
			}
			cert, err := tls.LoadX509KeyPair(certPath, keyPath)
			if err != nil {
				return nil, invalidFields("VPN 证书或私钥无法加载："+err.Error(), "vpn-mode", "vpn-tls-cert", "vpn-tls-key")
			}
			p.Config.TLS.Certificates = []tls.Certificate{cert}
		}
		p.Config.Address = get("vpn-listen")
		p.Config.TCPListen = get("vpn-tcp-listen")
		if p.Config.Address == "" && p.Config.TCPListen == "" && p.Config.UDPListen == "" {
			return nil, invalidFields("至少配置一个 VPN 监听：vpn-listen（QUIC）、vpn-udp-listen（UDP）或 vpn-tcp-listen（TCP/WSS）。", "vpn-listen", "vpn-udp-listen", "vpn-tcp-listen")
		}
		if p.Config.UDPListen != "" {
			for _, other := range []string{p.Config.Address, get("quic-listen")} {
				_, a, e1 := net.SplitHostPort(other)
				_, b, e2 := net.SplitHostPort(p.Config.UDPListen)
				if e1 == nil && e2 == nil && a == b && a != "0" {
					return nil, invalidFields("vpn-udp-listen 与 QUIC UDP 监听端口冲突，请使用不同 UDP 端口。", "vpn-udp-listen")
				}
			}
		}
		p.Config.MaxClients = fs.Lookup("vpn-max-clients").Value.(flag.Getter).Get().(int)
		p.Config.AllowPrivate = get("vpn-allow-private") == "true"
		if q := get("quic-listen"); q != "" && p.Config.Address != "" {
			_, a, e1 := net.SplitHostPort(q)
			_, b, e2 := net.SplitHostPort(p.Config.Address)
			if e1 == nil && e2 == nil && a == b {
				return nil, invalidFields("vpn-listen 与 quic-listen 不能占用同一个 UDP 端口；请为独立 VPN 配置不同端口。", "vpn-mode", "vpn-listen")
			}
		}
	} else {
		if err := setServerURL(p, get("vpn-server")); err != nil {
			return nil, err
		}
		{
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
				if e != nil && p.Config.Transport != "udp" {
					return nil, invalidFields("VPN CA 文件无法读取："+e.Error(), "vpn-mode", "vpn-tls-ca")
				}
				if !roots.AppendCertsFromPEM(b) && p.Config.Transport != "udp" {
					return nil, invalidFields("VPN CA 文件中没有有效的 PEM 证书。", "vpn-mode", "vpn-tls-ca")
				}
			}
			p.Config.TLS.RootCAs = roots
		}
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
		if !slices.Contains(LiveFields(p.Role), s.Key) && s.Key != "vpn-server" {
			values[s.Key] = get(s.Key)
		}
	}
	for _, k := range []string{"key-file", "tls-cert", "tls-key", "tls-ca"} {
		values[k] = get(k)
	}
	b, _ := json.Marshal(values)
	h := sha256.New()
	h.Write(b)
	h.Write(key)
	h.Write(p.Config.accountRevision[:])
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
	p.baseFingerprint = hex.EncodeToString(h.Sum(nil))
	p.Fingerprint = p.baseFingerprint
	if p.Role == "client" {
		p.Fingerprint = endpointFingerprint(p.baseFingerprint, get("vpn-server"))
	}
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
