package admin

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"sgate/internal/cli"
)

// 配置规则随表单下发；单位和边界不依赖部署文件中是否已经写好注释。
type configRule struct {
	Options  map[string]configRule `json:"options,omitempty"`
	Live     bool                  `json:"live,omitempty"`
	Type     string                `json:"type"`
	Hint     string                `json:"hint"`
	Min      string                `json:"min,omitempty"`
	Max      string                `json:"max,omitempty"`
	Choices  []string              `json:"choices,omitempty"`
	Required bool                  `json:"required,omitempty"`
}

func configurationRules() map[string]configRule {
	rules := map[string]configRule{}
	add := func(keys string, rule configRule) {
		for _, key := range strings.Fields(keys) {
			rules[key] = rule
		}
	}
	add("heartbeat heartbeat-timeout noise-interval noise-interval-min noise-interval-max noise-idle-after reconnect-min reconnect-max offline-timeout session-ttl", configRule{
		Type: "duration", Min: "0", Max: "9223372036854775807",
		Hint: "单位必须填写：ns、us/µs、ms、s、m、h，可组合如 1m30s；0 的含义依参数而定。",
	})
	add("admin_session_timeout", configRule{Type: "duration", Min: "1000000000", Max: strconv.FormatInt(int64(365*24*time.Hour), 10), Hint: "时间单位 s/m/h 等，例如 12h；范围 1s～8760h。"})
	add("noise-min-bytes noise-max-bytes", configRule{Type: "bytes", Min: "0", Max: "10485760", Hint: "单位为字节，支持 K/KB/KiB、M/MB/MiB；1K=1024，1M=1048576；上限 10M，0 使用默认值。"})
	add("quic-stream-window quic-connection-window quic-stream-buffer quic-replay-bytes quic-receive-bytes memory-budget-bytes", configRule{Type: "bytes", Min: "0", Max: "1099511627776", Hint: "受管缓存或 QUIC 窗口，支持 K/M；0 使用默认值，进程预算 0 表示不限制。"})
	for _, key := range []string{"noise-min-bytes", "noise-max-bytes"} {
		rule := rules[key]
		rule.Hint += " WS/WSS 表示每批字节；QUIC 表示每秒噪音负载字节，按小帧平滑发送，每条隧道独立计速。"
		rules[key] = rule
	}
	add("buffer-bytes", configRule{Type: "integer", Min: "0", Max: strconv.Itoa(int(^uint(0) >> 1)), Hint: "单位为字节，只填整数，不支持 K/M 后缀；0 使用默认 4194304，否则至少 16409。"})
	add("max-streams max-sessions", configRule{Type: "integer", Min: "0", Max: strconv.Itoa(int(^uint(0) >> 1)), Hint: "单位为个，填写非负整数；0 使用默认 128。"})
	add("admin_max_sessions", configRule{Type: "integer", Min: "1", Max: "10000", Hint: "单位为个，范围 1～10000。"})
	add("master_maxproc master_prefork", configRule{Type: "integer", Min: "1", Max: "1", Hint: "单位为进程数；sgate 托管模式仅支持 1。"})
	add("master_wakeup", configRule{Type: "seconds", Min: "0", Max: "2147483647", Hint: "单位为秒，只填整数；sock 服务可填 - 表示关闭。"})
	add("app_use_limit", configRule{Type: "integer", Min: "0", Max: "2147483647", Hint: "填写非负整数；app_use_limit 单位为次，app_threads 单位为线程数；0 保留框架默认行为。"})
	add("app_threads", configRule{Type: "integer", Min: "-1", Max: "2147483647", Hint: "单位为线程数；-1 或 0 保持当前运行设置。"})
	add("app_idle_limit app_wait_limit", configRule{Type: "integer", Min: "0", Max: "2147483647", Hint: "单位为秒，只填非负整数，不带 s 后缀。"})
	add("vpn-noise-traffic vpn-noise-idle vpn-manage-network vpn-block-ipv6 vpn-allow-private admin_enabled admin-enabled send-addr allow-reverse noise-traffic noise-idle noise-empty", configRule{Type: "boolean", Choices: []string{"true", "false", "1", "0", "t", "f", "T", "F", "TRUE", "FALSE", "True", "False"}, Hint: "布尔值：true 开启，false 关闭。"})
	add("master_disable master_reuseport master_status_notify master_unpriv master_chroot master_stop_kill master_stop_wait master_private master_check_fds master_check_mem master_check_cpu master_check_io master_check_limit app_quick_abort app_reuseport", configRule{Type: "boolean", Choices: []string{"yes", "no", "true", "false", "y", "n", "1", "0"}, Hint: "布尔值：yes/true/y/1 开启，no/false/n/0 关闭。"})
	for _, key := range strings.Fields("master_reuseport master_status_notify master_unpriv master_chroot") {
		rules[key] = configRule{Type: "boolean", Choices: []string{"no", "false", "n", "0"}, Hint: "sgate 托管模式要求关闭：no/false/n/0。"}
	}
	add("quic-initial-packet-bytes", configRule{Type: "integer", Min: "1200", Max: "1452", Hint: "QUIC 初始 UDP 负载，1200～1452 字节；后续仍自动探测路径 MTU。"})
	add("vpn-noise-rate", configRule{Type: "bytes", Min: "0", Max: "10485760", Hint: "本端每条 VPN 连接每秒噪音负载字节，支持 K/M；0 关闭，上限 10M。拥塞或业务排队时让出，不补发暂停额度。"})
	add("vpn-noise-idle-after", configRule{Type: "duration", Min: "0", Max: "9223372036854775807", Hint: "连续无双向业务 IP 数据的空闲阈值，支持 ms/s/m/h；0 使用 1s。"})
	add("vpn-mode", configRule{Type: "enum", Choices: []string{"disabled", "both", "only"}, Hint: "关闭 / 代理与 VPN 共存 / 仅 VPN；客户端应用配置后需手动启动 VPN，服务端自动监听。"})
	add("vpn-queue-max-age", configRule{Type: "duration", Min: "0", Max: "60000000000", Hint: "发送队列过期时间，0 关闭过期丢弃（默认），最多 1m；弱网 TCP 建议保持 0，队列容量仍有限制。"})
	add("vpn-udp-send-interval", configRule{Type: "duration", Min: "0", Max: "10000000", Hint: "纯 UDP VPN 每条连接发送大包的最小间隔，0 关闭，最多 10ms。"})
	add("vpn-mtu", configRule{Type: "integer", Min: "576", Max: "1500", Hint: "IPv4 TUN MTU，默认 1280 字节。"})
	add("vpn-max-clients", configRule{Type: "integer", Min: "1", Max: "254", Hint: "同时认证客户端数，1～254；在线调整仅限制新接入，不踢现有客户端。"})
	add("vpn-listen", configRule{Type: "listen", Hint: "VPN 独立 UDP 地址；启用 TCP 监听后可留空，不能与 quic-listen 共用 UDP 端口。"})
	add("vpn-udp-listen", configRule{Type: "listen", Hint: "纯 UDP VPN 监听，与 QUIC 分开 UDP 端口；业务包不重传，使用共享密钥加密。"})
	add("vpn-tcp-listen", configRule{Type: "listen", Hint: "可选 TLS/TCP 与 WSS 共用监听，可与 VPN UDP 使用同一数字端口；两种监听至少填写一个。"})
	add("vpn-tcp-notsent-lowat", configRule{Type: "bytes", Min: "0", Max: "1048576", Hint: "Linux VPN TCP/WSS 套接字未发送数据水位，支持 K/M，0 使用系统默认，最大 1M。"})
	add("vpn-server", configRule{Type: "vpn-url", Hint: "udp://、quic://、tcp:// 或 wss://主机:端口/vpn；UDP 对应 vpn-udp-listen，TCP/WSS 对应 vpn-tcp-listen。"})
	add("vpn-network vpn-client-ip vpn-routes vpn-dns vpn-tun-name vpn-dns-service vpn-egress-interface", configRule{Type: "text", Hint: "由 VPN 配置校验器检查；保存不创建网络设备，应用后生效。"})
	add("vpn-key-file vpn-tls-cert vpn-tls-key vpn-tls-ca", configRule{Type: "file", Hint: "VPN 文件路径；留空复用同用途的代理密钥或 TLS 配置。"})
	add("quic-mode", configRule{Type: "enum", Choices: []string{"native", "legacy"}, Hint: "QUIC 原生多流或旧版单流协议；修改后重启代理。"})
	add("master_type", configRule{Type: "enum", Choices: []string{"sock", "inet", "unix"}, Hint: "服务套接字类型：sock、inet 或 unix。"})
	add("listen admin_listen", configRule{Type: "listen", Required: true, Hint: "格式为 主机:端口；IPv6 使用 [地址]:端口；端口为 0～65535，0 表示自动分配。"})
	add("quic-listen", configRule{Type: "listen", Hint: "可选 UDP 监听地址，需配置 tls-cert/tls-key；更改后重启进程。"})
	add("target", configRule{Type: "address", Required: true, Hint: "IP或域名:端口，端口范围 1～65535；IPv6 使用 [地址]:端口。"})
	add("addr", configRule{Type: "address", Hint: "留空保留 URL 中的 addr；URL 也无 addr 时使用默认后端。填写 IP或域名:端口，端口范围 1～65535。"})
	add("server", configRule{Type: "url", Hint: "ws://、wss:// 或 quic:// URL，例如 wss://example.com:443/tunnel；配置多端口映射时可留空。"})
	add("allowed-targets", configRule{Type: "addresses", Hint: "逗号分隔的 IP或域名:端口；端口范围 1～65535；留空禁止客户端选后端。"})
	add("master_service", configRule{Type: "listeners", Required: true, Hint: "框架监听格式：IP|端口，多项用逗号分隔，IPv6 用 [地址]|端口；TCP 端口范围 1～65535。unix 类型使用绝对套接字路径。"})
	add("path", configRule{Type: "route", Hint: "以 / 开头的 URL 路径，不含 ? 或 #；留空使用 /tunnel；不能占用 /admin 管理路径。"})
	add("username", configRule{Type: "username", Max: "128", Hint: "隧道认证用户名；1～128 个 UTF-8 字节，不含空白或控制字符；留空使用旧单密码模式。"})
	add("password", configRule{Type: "password", Min: "1", Max: "1024", Hint: "1～1024 个 UTF-8 字节；留空不修改已保存的密码。"})
	add("proxies-file accounts-file key-file tls-ca tls-cert tls-key admin-config admin_web_dir admin_users_file admin_session_dir admin_setup_token_file admin_tls_cert admin_tls_key admin_master_ctl admin_master_socket master_command master_home master_log master_stdout master_stderr tls_cert_file tls_key_file app_queue_dir", configRule{Type: "file", Hint: "文件或目录路径；相对路径按该参数原有规则解析，文件存在性和权限由服务端校验。"})
	add("accounts-file", configRule{Type: "file", Hint: "账号 JSON 路径，相对本 .cf 目录解析；保存后在线加载，无需重启。清空恢复已生效的公共密码认证；命令行参数优先。"})
	for _, key := range strings.Fields("admin_web_dir admin_users_file admin_session_dir admin_setup_token_file master_command") {
		rule := rules[key]
		rule.Required = true
		rules[key] = rule
	}
	add("master_args master_env master_owner master_version master_notify_addr master_notify_recipients app_access_allow", configRule{Type: "text", Hint: "按服务模板格式填写；框架原始值不支持 #、首尾空白或换行。"})
	rules["master_args"] = configRule{Type: "arguments", Hint: "附加命令行参数；按空白拆分，不解析 shell 引号。布尔参数用 -选项=false；JSON 须紧凑且不带空白。留空保留框架自动参数。"}
	add("proxies", configRule{Type: "proxies", Hint: "最多 64 个映射；监听端口不可重复；删除全部映射后使用单代理配置。"})
	for key, zero := range map[string]string{
		"heartbeat": "0 使用默认 20s。", "heartbeat-timeout": "0 使用心跳间隔的三倍。",
		"noise-interval":     "0 关闭固定间隔发送；随机范围另行配置。",
		"noise-interval-min": "与 max 成对设置；均为 0 关闭随机发送。",
		"noise-interval-max": "须不小于 min；固定间隔非零时 min/max 均须为 0。",
		"reconnect-min":      "0 使用默认 250ms。", "reconnect-max": "0 使用默认 10s。",
		"offline-timeout": "0 使用默认 2m。", "session-ttl": "0 使用默认 2m。",
	} {
		rule := rules[key]
		rule.Hint += " " + zero
		rules[key] = rule
	}
	for key, hint := range map[string]string{
		"noise-traffic":    "true 在业务传输中混入噪音；false 暂停该场景噪音，不影响心跳。",
		"noise-idle":       "true 在有业务连接但双向无业务数据的间隙发送；间隔及大小沿用白噪音参数。",
		"noise-empty":      "true 在无业务连接但隧道在线时发送；false 暂停该场景噪音。",
		"noise-idle-after": "单位 ms/s/m/h；双向连续无业务数据达到此时间算空闲，0 使用默认 1s；新建且从未传输的业务连接视为空闲。",
	} {
		rule := rules[key]
		rule.Hint = hint
		rules[key] = rule
	}
	for _, key := range []string{"noise-interval", "noise-interval-min", "noise-interval-max"} {
		rule := rules[key]
		rule.Hint += " QUIC 中控制随机目标速率的重选周期（至少 1s），不再表示整批发送后的停顿；全部为 0 时关闭噪音。"
		rules[key] = rule
	}
	for key, rule := range rules {
		switch rule.Type {
		case "integer", "seconds", "duration", "bytes", "boolean", "enum":
			rule.Required = true
			rules[key] = rule
		}
	}
	return rules
}

var configRules = configurationRules()

func ruleForConfig(key string) configRule {
	if rule, ok := configRules[key]; ok {
		return rule
	}
	return configRule{Type: "text", Hint: "按配置项原有格式填写；不能包含换行或空字符。"}
}

// 前端用于即时反馈；服务端再次校验编辑项，直接调用 API 不能绕过基本类型约束。
func validateConfigEdit(key, value string) error {
	rule := ruleForConfig(key)
	if value == "" {
		if rule.Required {
			return errors.New("不能为空")
		}
		return nil
	}
	var number int64
	var err error
	switch rule.Type {
	case "integer", "seconds":
		if rule.Type == "seconds" && value == "-" {
			return nil
		}
		number, err = strconv.ParseInt(value, 10, 64)
	case "bytes":
		var n int
		n, err = cli.ParseByteSize(value)
		number = int64(n)
	case "duration":
		var d time.Duration
		d, err = time.ParseDuration(value)
		number = int64(d)
	case "boolean", "enum":
		for _, choice := range rule.Choices {
			if value == choice {
				return nil
			}
		}
		return errors.New("请使用允许的选项")
	case "address", "listen":
		return validateFormAddress(value, rule.Type == "listen")
	case "addresses":
		for _, address := range strings.Split(value, ",") {
			if err := validateFormAddress(strings.TrimSpace(address), false); err != nil {
				return err
			}
		}
		return nil
	case "vpn-url":
		u, e := url.Parse(value)
		if e != nil || (u.Scheme != "quic" && u.Scheme != "tcp" && u.Scheme != "wss" && u.Scheme != "udp") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/vpn") || u.Hostname() == "" || u.Port() == "" {
			return errors.New("需要 udp://、quic://、tcp:// 或 wss://主机:端口/vpn 地址")
		}
		return validateFormAddress(u.Host, false)
	case "url":
		u, e := url.Parse(value)
		if e != nil || (u.Scheme != "ws" && u.Scheme != "wss" && u.Scheme != "quic") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return errors.New("需要有效的 ws://、wss:// 或 quic:// URL")
		}
		if u.Scheme == "quic" && u.Port() == "" {
			return errors.New("QUIC URL 必须指定 UDP 端口")
		}
		if u.Port() != "" {
			if _, e := strconv.ParseUint(u.Port(), 10, 16); e != nil || u.Port() == "0" {
				return errors.New("URL 端口范围为 1～65535")
			}
		}
		return nil
	case "route":
		if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "?#\t ") {
			return errors.New("需要不含查询参数或片段的绝对 URL 路径")
		}
		return nil
	case "username":
		if len(value) > 128 || strings.IndexFunc(value, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) }) >= 0 {
			return errors.New("用户名不能超过 128 个 UTF-8 字节，且不能含空白或控制字符")
		}
		return nil
	case "password":
		if len(value) > 1024 {
			return errors.New("密码不能超过 1024 个 UTF-8 字节")
		}
		return nil
	default:
		return nil
	}
	if err != nil {
		return errors.New("格式错误；" + rule.Hint)
	}
	min, _ := strconv.ParseInt(rule.Min, 10, 64)
	max, _ := strconv.ParseInt(rule.Max, 10, 64)
	if key == "buffer-bytes" && number > 0 && number < 16409 {
		return errors.New("缓存至少为 16409 字节，或填 0 使用默认值")
	}
	if number < min || number > max {
		return fmt.Errorf("超出允许范围；%s", rule.Hint)
	}
	return nil
}

func validateFormAddress(value string, listen bool) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil || (!listen && host == "") || strings.ContainsAny(host, "/?# \t") {
		return errors.New("需要有效的 主机:端口 地址")
	}
	number, e := strconv.Atoi(port)
	if e != nil || number > 65535 || number < 0 || (!listen && number == 0) {
		return errors.New("端口超出范围或不是整数")
	}
	return nil
}

// 只发布参数类型，不包含命令行的真实取值或密码。
func argumentOptions(role string) map[string]configRule {
	options := map[string]configRule{}
	clientOnly := " vpn-server vpn-client-ip vpn-tls-ca vpn-routes vpn-dns vpn-dns-service vpn-block-ipv6 quic-mode username proxies proxies-file server addr send-addr allow-reverse tls-ca reconnect-min reconnect-max offline-timeout "
	serverOnly := " vpn-listen vpn-udp-listen vpn-tcp-listen vpn-tls-cert vpn-tls-key vpn-max-clients vpn-egress-interface vpn-allow-private quic-listen accounts-file target allowed-targets path tls-cert tls-key session-ttl max-sessions "
	for key, rule := range configRules {
		if strings.Contains(key, "_") || key == "master_args" || strings.HasPrefix(key, "app_") {
			continue
		}
		if role == "client" && strings.Contains(serverOnly, " "+key+" ") {
			continue
		}
		if role == "server" && strings.Contains(clientOnly, " "+key+" ") {
			continue
		}
		rule.Options = nil
		options[key] = rule
	}
	options["config"] = configRule{Type: "file"}
	for _, key := range []string{"alone", "v", "u", "c"} {
		options[key] = configRule{Type: "boolean", Choices: []string{"true", "false", "1", "0", "t", "f", "T", "F", "TRUE", "FALSE", "True", "False"}}
	}
	for _, key := range []string{"f", "n", "t"} {
		options[key] = configRule{Type: "text"}
	}
	options["s"] = configRule{Type: "integer", Min: "1", Max: "256"}
	return options
}
