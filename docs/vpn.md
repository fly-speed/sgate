# 独立 IPv4 VPN：sgate-vpn

`sgate-vpn` 是 sgate 新增的独立 VPN 入口，不依赖 OpenVPN。原有 `sgate-client` / `sgate-server` TCP 代理继续保留。VPN 使用独立 UDP 监听及 ALPN `sgate-vpn-v1`，IP 数据通过 QUIC DATAGRAM 发送，不复用有序业务流；认证控制流不承载 IP 数据。

首版支持 Linux 服务端、macOS/Linux 客户端、IPv4、固定客户端虚拟地址、多客户端、自动 TUN/路由/DNS、Linux 转发/NAT、退出清理和重连。除独立 CLI 外，现已集成至 `sgate-client` / `sgate-server`，可通过现有管理网页配置并随服务进程运行。Windows、IPv6 转发、按账号授权和在线配置不在此版本内。

## 在原客户端、服务端及网页中使用（推荐）

执行 `make build` 后更新两端 `sgate-client`、`sgate-server` 和 `web/admin/`，首次升级需重启进程加载新参数。新版 `sgate-client.cf`、`sgate-server.cf` 以及各用途的客户端/服务端 `.cf` 示例均完整列出各端适用的 `vpn-*` 参数、默认值和说明，默认保持关闭，可直接编辑同一份配置文件，无需另外创建 VPN 配置。进入管理网页的“配置”页，展开“独立 VPN”分组；即使旧 `.cf` 没有 VPN 配置，也会显示默认字段，保存时仅补入实际提交的项，不产生重复键。

`vpn-mode` 有三个值：`disabled` 默认关闭，忽略配置文件中其余 `vpn-*` 参数（包括另一端的参数和未使用的无效值）；`both` 同时运行原代理和独立 VPN；`only` 仅 VPN。启用 VPN 时仍严格校验本端适用的配置项；非 VPN 参数、重复项和配置结构错误始终会报错。`only` 不要求代理目标、上游地址或代理账号密码，忽略代理映射/账号文件，客户端界面不加载或编辑代理映射；保留框架的 TCP/HTTP 管理监听，客户端业务 TCP 立即关闭，服务端代理 HTTP 返回 404。服务端管理仍位于 `/admin/`。管理端口与 VPN 的 UDP 端口是不同入口。

在**服务端**填写 `vpn-listen = 0.0.0.0:8444`、`vpn-network = 10.77.0.0/24`、`vpn-mtu = 1280`，选择所需模式。`vpn-tls-cert` / `vpn-tls-key` 可分别指定 VPN 证书，留空复用 `tls-cert` / `tls-key`。**客户端**填写 `vpn-server = quic://你的证书域名:8444/vpn`、`vpn-client-ip = 10.77.0.2`，配置相同子网/MTU、`vpn-dns` 和正确的 `vpn-dns-service`。自签名证书用 `vpn-tls-ca`（留空复用 `tls-ca`）。两端 `vpn-key-file` 必须指向相同密钥，留空复用 `key-file`。

VPN 参数统一以 `vpn-` 开头，不使用独立 CLI 示例中的 `role`、`server`、`listen` 等同名项来管理 VPN。`vpn-listen` 不能占用 `quic-listen` 的 UDP 端口。旧 `vpn-client.cf` / `vpn-server.cf` 仍是 OpenVPN 的 TCP 代理示例。

切换 `vpn-mode` 失败时，界面会在参数旁及全局提示中显示原因，并保留未保存的修改；不支持的操作系统会即时说明，后端校验失败会指出不适用于本端的 VPN 参数、缺少的证书/私钥或地址等问题。macOS 可用作客户端，不能启用独立 VPN 服务端。

点击现有“保存并应用”按钮后生效；单独保存会保留待应用状态。客户端应用配置会断开原代理/VPN连接，恢复旧网络后按新配置重建；服务端共享管理入口保持在线，VPN 参数未改变时保留现有 VPN，改变时停止旧 VPN 并启动新 VPN。`disabled` 停用并恢复网络。CLI 对同名参数的覆盖仍优先，界面会标注。

实时概览显示 VPN 的启动/连接/重连/失败状态、TUN、监听或远端地址、虚拟地址、活跃连接、IP 收发字节及丢弃数。带宽图选择 `VPN IP` 查看全部客户端的 VPN IP 负载（不含 QUIC/UDP/IP 封装开销，不按原代理的 IP 筛选）；原 TCP/QUIC 代理曲线保持其原有统计口径。VPN 重建时它自己的计数清零。

运行权限不足、端口被占用或路由/DNS 操作失败时，VPN 状态会显示错误，管理网页继续运行；“配置已应用”不代表 VPN 已连通，应检查实时状态。修复后再次应用可重试，客户端连接失败会自动退避重连。配置候选校验不会创建 TUN、修改路由或启动 VPN。

独立 VPN 的权限要求同样适用于集成模式：服务端 Linux；客户端 macOS/Linux；进程需要 TUN 与网络管理权限（现有模板关闭降权/chroot）。通过网页更改路由/DNS 属于本机网络管理操作，开启前先停止其他全隧道 VPN。安装及本次自动化验证不会启用 VPN，也不会改动当前网络。

## 编译、安装

```sh
make vpn
sudo make install-vpn
```

`make build` 也会编译 VPN；全量 `make install` 在找到 VPN 二进制时一并安装。安装不启动服务，不修改网络。文件位置：

- `${PREFIX}/sbin/sgate-vpn`
- `${PREFIX}/samples/sgate-vpn-server.conf` 与 `sgate-vpn-client.conf`
- `${PREFIX}/docs/vpn.md` 和 `${PREFIX}/bin/test-vpn-linux.py`
- `${PREFIX}/conf/sgate.key`，可改成专门生成的 VPN 密钥文件。

`PREFIX` 默认为 `/opt/soft/sgate`。示例保留已有文件，更新内容另存 `.dist`。复制所需示例到 `conf/` 后编辑，不要把旧 TCP 代理 `.cf` 直接交给此程序。

## 美国 Linux 服务端

要求 root 或具备对应管理权限、`/dev/net/tun`、iproute2（`ip`）、`sysctl` 和 `iptables`（兼容 nft 后端也可）。放行云防火墙和主机防火墙的 UDP 8444。该 UDP 端口不能同时被旧 `sgate-server` 占用；TCP 同端口不冲突。

```ini
role = server
listen = 0.0.0.0:8444
key-file = /opt/soft/sgate/conf/vpn.key
tls-cert = /你的真实证书/server.pem
tls-key = /你的真实证书/server-key.pem
vpn-network = 10.77.0.0/24
mtu = 1280
max-clients = 64
manage-network = true
egress-interface = ''
allow-private = false
```

```sh
sudo /opt/soft/sgate/sbin/sgate-vpn -config /opt/soft/sgate/conf/sgate-vpn-server.conf
```

自动建立新 `sgate0`，服务器虚拟地址为子网第一个主机（默认 `10.77.0.1`），按当前路由识别出口；必要时显式设置 `egress-interface = eth0` 或实际网卡名。启用 IPv4 forwarding，建立只属于此进程的 FORWARD/NAT 链，并允许到虚拟网关的 ICMP echo。退出删除本次规则，恢复原 forwarding 值，不清空既有防火墙规则。若主机上有其他服务同时修改 forwarding，需协调维护其全局状态。

`allow-private=false` 拒绝访问服务器侧私网、回环、链路本地、多播以及其他 VPN 客户端地址；服务器虚拟网关本身允许访问。VPN 地址不得与已有网络重叠。现有防火墙/策略路由仍可能影响转发，须在实际 Linux 环境验证。

## 中国 macOS 客户端

先停止 OpenVPN 或其他接管全隧道路由的 VPN；程序遇到现存路由冲突会失败并回滚，不覆盖另一 VPN 的路由。

```ini
role = client
server = quic://你的证书域名:8444/vpn
key-file = /opt/soft/sgate/conf/vpn.key
tls-ca = ''
vpn-network = 10.77.0.0/24
client-ip = 10.77.0.2
mtu = 1280
tun-name = ''
manage-network = true
routes = 0.0.0.0/1,128.0.0.0/1
dns = 1.1.1.1
dns-service = Wi-Fi
block-ipv6 = true
```

两端使用相同的 32 字节密钥（sgate 的 64 位十六进制文本格式）。证书主机名必须匹配 `server`；自签名证书通过 `tls-ca` 显式信任，不提供跳过校验选项。随包测试证书只覆盖 localhost/127.0.0.1/::1，不能直接用于美国服务器域名。

```sh
sudo /opt/soft/sgate/sbin/sgate-vpn -config /opt/soft/sgate/conf/sgate-vpn-client.conf
```

自动创建一个新的 `utunN`，保护服务端 IPv4 地址的物理出口路径，添加两条 IPv4 /1 路由，然后设置 DNS。`dns-service` 必须是实际网络服务名称，例如 Wi-Fi 或以太网服务，可用 `networksetup -listallnetworkservices` 查询。程序不会同时修改其他网络服务的 DNS。

退出时恢复先前的手动 DNS；原为 DHCP DNS 时恢复自动获取；删除自己添加的路由，释放 utun。普通 Ctrl-C/SIGTERM 可执行恢复；断电、SIGKILL、内核故障无法保证用户态清理，尤其应检查 DNS 是否仍指向 VPN 内指定解析器。`manage-network=false` 可用于由外部运维工具管理网络的场景，此时需自行配置设备、路由、DNS 和转发。

Linux 客户端使用新 `sgate0`，自动 DNS 需要 `resolvectl` / systemd-resolved；若系统未使用它，应设置 `dns = ''` 并由运维自行配置解析器。IPv4 LAN 的更具体路由仍保留，不宣称隔离本地局域网。

首版不转发 IPv6。默认把 IPv6 的两个 /1 路由导向 TUN 后丢弃，避免公网 IPv6 走物理默认路由；这是 IPv6 阻断，不是 IPv6 VPN 支持。`block-ipv6=false` 仅适用于操作者已自行处理 IPv6 路由的环境。

## 连接、报文与权限边界

- 每个同时在线客户端需要唯一 `client-ip`，如 `.2`、`.3`。虚拟地址和 MTU 在两端校验，不能使用网络/广播/服务器地址。
- TLS 1.3 验证服务器证书；控制流使用随机挑战和共享密钥 HMAC 进行双向确认。未认证报文不会写入 TUN。不使用 0-RTT 发送 IP 包。
- 当前是一个共享密钥信任域，持有密钥即获 VPN 准入；不是既有 Web 管理账号或 TCP 账号 ACL，也没有单用户撤销功能。
- 服务端校验每个报文的源地址必须等于该连接绑定地址，禁止客户端互访；客户端校验目的地址必须为自己的虚拟地址。
- MTU 576～1500，默认 1280。一个 IP 包拆成最多两个 1000 字节负载片段，适应 QUIC 初始 1200 字节 UDP 负载；不把大 IP 包回退到可靠流。
- 每连接至多 64 个重组项，2 秒过期；发送队列 64 包。丢片只影响对应 IP 包，后续其他包仍可交付。拥塞/离线时丢弃，不建立 IP 重放缓存。
- QUIC 自身仍有拥塞控制，DATAGRAM 并不保证送达、顺序或性能；它消除了此层的可靠流重传等待，不能消除跨境 UDP 丢包和路径限速。
- 客户端保存虚拟 IP，断连后退避重连，不重新应用路由。网络配置失败直接退出并回滚，避免无限重试特权命令。服务器地址在进程启动时解析并固定，域名切换 IP 后需重启客户端。
- VPN 首版没有业务白噪音或 WSS 回退，保持 QUIC 心跳。每 10 秒记录 IP 收发字节、拒绝/丢弃、连接数、最近平滑 RTT、QUIC 丢包判定和 PTO 事件。RTT 是最近连接采样，不是业务 ping 延迟；TX 是交给 QUIC 的 IP 负载，不代表对端已收到。

## 验证状态

本机使用内存 TUN 和真实 UDP/QUIC 套接字验证了双向 IPv4 包（包含 1280 字节分片）、重连、身份与证书拒绝、地址冲突、源地址伪造/私网隔离、丢片与乱序独立交付及内存边界。集成模式另验证旧配置的网页升级、输入校验、仅 VPN 的代理拒绝、运行错误展示及内存 TUN 下的 VPN 监听/退出；`python3 scripts/test-vpn-management.py` 使用真实客户端进程与认证 API 验证保存待应用、应用/重试/停用、缺失的未使用映射文件和权限/设备错误展示，刻意使用无效设备名，不创建 TUN。macOS 路由/DNS 使用命令替身验证创建、失败回滚和恢复，不会改动当前 OpenVPN 或系统网络。

**尚未执行 macOS 特权 utun/路由/DNS 实测，以及 Linux 内核 TUN/NAT/实际互联网端到端实测。** Linux 代码的交叉编译不代表运行验证。按此前约定，没有 Linux 环境时交付测试脚本并明确标记未实测。

Linux 隔离测试脚本在三个临时网络命名空间内运行客户端、服务端和模拟互联网端，检查真实 TCP 下载、UDP 回显、ICMP、外层端点路由排除，以及停止后的路由、防火墙和 forwarding 恢复。不会修改宿主机路由/DNS/防火墙；需要 Linux、root、TUN、ip、iptables、sysctl、openssl、ping，弱网模式还需要 tc：

```sh
sudo python3 scripts/test-vpn-linux.py \
  --binary bin/sgate-vpn --output artifacts/vpn-linux-clean

sudo python3 scripts/test-vpn-linux.py \
  --binary bin/sgate-vpn --output artifacts/vpn-linux-200ms \
  --delay-ms 100 --loss-percent 0.1
```

也可直接验证集成后的两个程序（同样尚未在 Linux 执行）：

```sh
sudo python3 scripts/test-vpn-linux.py \
  --client-binary bin/sgate-client --server-binary bin/sgate-server \
  --output artifacts/vpn-integrated-linux
```

安装后也可使用 `${PREFIX}/bin/test-vpn-linux.py --binary ${PREFIX}/sbin/sgate-vpn --output <新目录>`。选择不存在的输出目录，保留 `verification.json` 和子进程日志。默认 IPv4 测试不设置 DNS，因此 Linux 脚本不替代真实客户端 DNS 验证。命名空间或内核权限不足时测试会失败，不应把脚本交付视为已通过。

完成系统级验证后，再在中美实际链路比较旧 OpenVPN 组合与新 VPN 的空闲/下载中 ping、业务吞吐、CPU 和数据报丢弃指标。
