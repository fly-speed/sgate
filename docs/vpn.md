# 独立 IPv4 VPN：sgate-vpn

`sgate-vpn` 是 sgate 新增的独立 VPN 入口，不依赖 OpenVPN。原有 `sgate-client` / `sgate-server` TCP 代理继续保留。新增无业务包重传的纯 UDP 传输 `udp://`，详见 [UDP VPN 配置、协议和验证](vpn-udp.md)。VPN 默认使用独立 QUIC UDP 监听及 ALPN `sgate-vpn-v1`，IP 数据通过 DATAGRAM 发送。另支持 TLS/TCP 和 WSS 可靠传输，可同时监听并由客户端手动选择，均使用相同的共享密钥、证书校验和虚拟网络配置。

客户端的代理与独立 VPN 支持 macOS、Linux 和 Windows；VPN 服务端支持 macOS/Linux。支持 IPv4、服务端自动分配虚拟地址、多客户端、自动 TUN/路由/DNS、Linux 转发/NAT、macOS PF NAT、退出清理和重连。除独立 CLI 外，已集成至 `sgate-client` / `sgate-server`，可通过管理网页配置。IPv6 转发和按账号授权尚未提供。

服务端实时概览的“在线 VPN 客户端”表格显示已认证客户端的公网 IP（服务端观察到的连接源地址）、VPN IP 和 QUIC/TCP/WSS 协议。局域网或运营商 NAT 后的客户端显示其出口地址；经中继时显示中继地址，不信任客户端自报或 HTTP 转发头。连通测试不计入在线列表，连接关闭后自动移除。

## Windows 客户端

Windows 10/11 的客户端使用 [Wintun 官方签名驱动](https://www.wintun.net/)，需要匹配 CPU 架构的 `wintun.dll` 与 `sgate-client.exe` 放在同一 `bin` 目录。普通代理不加载该驱动；启用 VPN 需要以管理员身份启动程序。服务端仍运行在 Linux/macOS。

Windows 包还包含独立窗口程序 `bin/sgate-control.exe`，与 `sgate-client.exe` 是两个可执行程序。双击控制 APP，确认 Windows 管理员授权提示后，可点击“启动客户端”“停止客户端”，并查看进程 PID、停止进度及错误；“打开管理页面”进入原有代理/VPN 配置界面，“编辑配置”打开配置文件，“查看日志”打开 `logs/sgate-client.log`。客户端在后台运行，没有额外控制台窗口。控制 APP 本身不自动启动客户端，VPN 仍须在网页手动启动。

控制 APP 仅管理由本窗口启动的子进程，不接管其他窗口、脚本或服务启动的客户端。同一安装目录只允许一个控制窗口。停止或关闭窗口通过私有继承管道通知客户端正常退出，等待原有退出流程完成 VPN 清理后才显示“已停止”或关闭窗口；不会用强制终止冒充清理完成。控制 APP 意外退出时，管道关闭也会通知子进程退出。若客户端正在清理，窗口保留“正在停止”的状态。强制结束客户端或系统关机仍无法保证完成清理。新版控制 APP 与客户端应配套更新；旧客户端会因不支持控制参数而拒绝启动，避免启动一个无法正常停止的旧进程。

在已安装 Go 1.24 或更新版本的 Windows 上，从项目根目录运行打包入口：

```bat
package\build-windows.bat -Architecture amd64
package\build-windows.bat -Architecture arm64 -Destination "D:\sgate builds\arm64"
```

双击 `package\build-windows.bat` 会按默认参数打包 AMD64。批处理会调用同目录下的 `build-windows.ps1`，并传递参数和退出码。

脚本在 `dist/windows-client/` 生成客户端、独立控制 APP、管理页面、Windows 示例配置和 PowerShell 启动器，并下载固定版本 Wintun、校验官方 SHA256、附带驱动许可。已有配置和密钥保留；首次生成的本地随机密钥须替换成与服务器相同的共享密钥。构建需要联网获取 Go 依赖和 Wintun。

编辑包内 `conf/sgate-client.cf`，设置代理 `server`、认证参数、VPN `vpn-server` 和 CA；也可先启动后通过 `http://127.0.0.1:9081/` 管理。启动器将工作目录设置为包内 `conf`，确保共享密钥相对路径一致：

```powershell
.\dist\windows-client\scripts\start-windows-client.ps1
```

VPN 默认不自动启动，在网页配置启用模式后手动启动。Windows 创建独立 Wintun 网卡；认证分配地址后，先保留服务器外层 IP 的物理出口路由，再安装虚拟地址、隧道路由及 DNS。IPv6 阻断使用隧道黑洞路由。停止、重启或配置失败会逆序清理本次创建的设置，保留已有外层主机路由，物理网卡地址和 DNS 不作修改。`vpn-dns-service` 仅用于 macOS，Windows 自动使用新建的 Wintun 网卡。正常退出请先停止 VPN，再使用 Ctrl+C；强制终止无法保证执行清理。

验证情况：已在 macOS 进行 Windows amd64/arm64 交叉编译，以及 Windows 路由计划、出口保留、已有路由保护、DNS 失败回滚、命令参数编码和 IPv6 阻断测试。控制进程的启动、正常停止、等待清理、重复启动保护、重启、控制管道 EOF 和取消流程有进程级测试。没有 Windows 实机，原生窗口交互、管理员提升、Wintun 驱动加载和系统网络转发尚未实测。原有 macOS/Linux 客户端继续支持独立启动。

## 在原客户端、服务端及网页中使用（推荐）

执行 `make build` 后更新两端 `sgate-client`、`sgate-server` 和 `web/admin/`，首次升级需重启进程加载新参数。新版 `sgate-client.cf`、`sgate-server.cf` 以及各用途的客户端/服务端 `.cf` 示例均完整列出各端适用的 `vpn-*` 参数、默认值和说明，默认保持关闭，可直接编辑同一份配置文件，无需另外创建 VPN 配置。进入管理网页的“配置”页，展开“独立 VPN”分组；即使旧 `.cf` 没有 VPN 配置，也会显示默认字段，保存时仅补入实际提交的项，不产生重复键。

`vpn-mode` 有三个值：`disabled` 默认关闭，忽略配置文件中其余 `vpn-*` 参数（包括另一端的参数和未使用的无效值）；`both` 同时运行原代理和独立 VPN；`only` 仅 VPN。启用 VPN 时仍严格校验本端适用的配置项；非 VPN 参数、重复项和配置结构错误始终会报错。`only` 不要求代理目标、上游地址或代理账号密码，忽略代理映射/账号文件，客户端界面不加载或编辑代理映射；保留框架的 TCP/HTTP 管理监听，客户端业务 TCP 立即关闭，服务端代理 HTTP 返回 404。服务端管理仍位于 `/admin/`。管理端口与 VPN 的 UDP 端口是不同入口。

在**服务端**填写 `vpn-listen = 0.0.0.0:8444`、`vpn-network = 10.77.0.0/24`、`vpn-mtu = 1280`，选择所需模式。`vpn-tls-cert` / `vpn-tls-key` 可分别指定 VPN 证书，留空复用 `tls-cert` / `tls-key`。**客户端**填写 `vpn-server = quic://你的证书域名:8444/vpn`，配置相同子网/MTU、`vpn-dns` 和正确的 `vpn-dns-service`。虚拟 IPv4 地址由服务端认证后自动分配，客户端实时概览显示“虚拟 IP”。客户端表单和示例不再提供 `vpn-client-ip`；旧文件中的该项兼容读取但忽略。自签名证书用 `vpn-tls-ca`（留空复用 `tls-ca`）。两端 `vpn-key-file` 必须指向相同密钥，留空复用 `key-file`。

VPN 参数统一以 `vpn-` 开头，不使用独立 CLI 示例中的 `role`、`server`、`listen` 等同名项来管理 VPN。`vpn-listen` 不能占用 `quic-listen` 的 UDP 端口。旧 `vpn-client.cf` / `vpn-server.cf` 仍是 OpenVPN 的 TCP 代理示例。

切换 `vpn-mode` 失败时，界面会在参数旁及全局提示中显示原因，并保留未保存的修改；不支持的操作系统会即时说明，后端校验失败会指出不适用于本端的 VPN 参数、缺少的证书/私钥或地址等问题。macOS 现已同时支持客户端与独立 VPN 服务端。

需要重建的参数点击“保存并应用”后生效，单独保存保留待应用状态；标注在线生效的参数保存即更新。客户端进程启动后不会自动启动 VPN；应用需要重建的配置会断开原代理/VPN连接并恢复旧网络，随后需在实时概览点击“启动 VPN”。启动使用已应用的配置，未保存或未应用的修改不参与启动。“停止 VPN”会等待连接退出、路由及 DNS 清理和 TUN 关闭，再返回结果；不会停止原代理或管理界面。启停状态不写入配置，进程重启后仍需手动启动。普通连接故障在手动启动后仍会自动重连，点击停止才结束重连并清理网络。服务端共享管理入口保持在线，VPN 静态参数未改变时保留现有 VPN；在线参数直接更新，静态参数改变并应用时停止旧 VPN 并启动新 VPN。`disabled` 停用并恢复网络。CLI 对同名参数的覆盖仍优先，界面会标注。

客户端“独立 VPN”分组在 `vpn-mode` 下提供“测试 VPN 连接”按钮；选择 `only` 或 `both` 后即可使用，不要求先保存或应用。测试使用当前表单中的 VPN 地址、证书/CA、密钥、子网、MTU 和虚拟 IP；命令行覆盖项仍优先。测试返回配置、DNS 解析、QUIC/TLS 或 TCP/TLS、认证或数据帧回显阶段的结果、耗时和错误，修改参数后隐藏旧结果。

测试按所选协议建立独立连接，完成证书/共享密钥认证与小型 VPN 数据帧回显，不创建 TUN、不配置路由/DNS、不保存配置、不占用或替换虚拟 IP。已有在线连接不会中断；虚拟 IP 已占用或服务端达到客户端上限时会单独提示，但连通性测试仍可成功。测试成功不代表 TUN、NAT、DNS 接管或互联网转发已经正常，完整 VPN 仍需实际业务验证。

无会话测试需要服务端也更新至支持该功能的版本。正常 VPN 的原有认证与数据协议保持不变；服务端挑战包含测试能力标识；较旧服务端未声明此标识时，认证失败会提示核对实际监听端口的程序版本和密钥。新版服务端会明确反馈共享密钥不匹配，并记录拒绝日志；子网和 MTU 不一致会返回服务端期望值。旧服务端会拒绝测试认证，客户端会提示检查密钥或升级服务端，不会退回建立普通 VPN 会话来测试。

实时概览显示 VPN 的启动/连接/重连/失败状态、TUN、监听或远端地址、虚拟地址、活跃连接、IP 收发字节及丢弃数。带宽图选择 `VPN IP` 查看全部客户端的 VPN IP 负载（不含 TLS、QUIC、WebSocket 及外层 TCP/UDP/IP 封装开销，不按原代理的 IP 筛选）；原 TCP/QUIC 代理曲线保持其原有统计口径。VPN 重建时它自己的计数清零。

运行权限不足、端口被占用或路由/DNS 操作失败时，VPN 状态会显示错误，管理网页继续运行；“配置已应用”不代表 VPN 已连通，应检查实时状态。修复后客户端点击“启动 VPN”重试，服务端再次应用可重试；客户端手动启动后连接失败会自动退避重连。配置候选校验不会创建 TUN、修改路由或启动 VPN。

独立 VPN 的权限要求同样适用于集成模式：服务端与客户端均支持 macOS/Linux；进程需要 TUN 与网络管理权限（现有模板关闭降权/chroot）。通过网页更改路由/DNS 属于本机网络管理操作，开启前先停止其他全隧道 VPN。安装及本次自动化验证不会启用 VPN，也不会改动当前网络。

## VPN 参数在线生效

客户端和服务端网页中的下列参数标注“保存后立即生效”，保存即更新正在运行的 VPN，不重建隧道、设备、路由或 DNS：

| 参数 | 适用端 | 在线行为 |
| --- | --- | --- |
| `vpn-noise-rate` | 两端 | 修改白噪音速率，包括从 0 开启、改为 0 关闭 |
| `vpn-noise-traffic` | 两端 | 修改业务传输期间的白噪音策略 |
| `vpn-noise-idle` | 两端 | 修改空闲时的白噪音策略 |
| `vpn-noise-idle-after` | 两端 | 修改业务空闲判定阈值 |
| `vpn-queue-max-age` | 两端 | 后续队列操作使用新期限；降低期限可能丢弃已排队的旧包，0 关闭过期 |
| `vpn-udp-send-interval` | 两端 | 调整纯 UDP VPN 本端每条连接的大包发送间隔，0 关闭，最多 10ms |
| `vpn-max-clients` | 服务端 | 新接入使用新上限，降低上限不主动断开现有会话 |
| `vpn-allow-private` | 服务端 | 后续双向转发使用新策略，关闭时阻止继续转发私网流量 |

`vpn-udp-send-interval` 只对本端发出的、长度至少 500 字节的 IPv4 业务包应用节奏控制。设置服务端影响下载方向，设置客户端影响上传方向。业务包仍由 UDP 单次发送。独立 `sgate-vpn` CLI 对应 `udp-send-interval`。

纯 UDP VPN 两端启动时尝试为各自的套接字申请 1 MiB 接收缓冲。实际容量受操作系统限制，申请失败时使用系统提供的容量。

客户端 `vpn-server` 单独标注“保存后自动重连”：运行中的 VPN 会先结束旧会话、完成旧路由和 DNS 清理，再连接新地址或协议；代理进程和管理界面保留，原 VPN 中的业务连接会中断。只更新地址与协议，不顺带应用其他待生效的 MTU、CA、密钥或路由设置。VPN 已停止时只更新下次启动的地址，不自动启动。切换复用客户端身份，避免旧 QUIC 会话尚未清理时发生虚拟 IP 占用冲突。连接失败会显示连接或重试错误，保存成功不代表连接已经成功。

其余 VPN 参数标注“应用后生效”，包括模式、监听地址、证书/密钥、子网、MTU、TUN、自动网络管理、出口网卡、路由、DNS 和 IPv6 阻断。“独立 VPN”分组新增“重启 VPN 使配置生效”按钮，只应用已保存的 VPN 配置并重建 VPN，保留管理登录、其他未保存草稿及待应用的代理配置。客户端点击该按钮属于显式启动或重启操作。`only` 与其他模式切换涉及代理运行方式，仍需使用全局“保存并应用”；专属重启按钮会明确提示原因。重启接口校验登录、CSRF 及配置版本。

普通在线参数保存不应用其他静态修改。重启表示已按保存配置发起重建，最终连接结果以实时概览为准。

命令行覆盖仍优先，界面会显示“命令行已覆盖”；修改磁盘值不会取代显式启动参数。VPN 停止或关闭时，保存在线项不会自动启动 VPN，后续启动使用已应用定义中的在线值。直接编辑配置文件不触发自动监视，以上在线更新指通过管理网页保存；独立 `sgate-vpn` CLI 没有网页配置热更新入口。旧客户端未协商白噪音能力时仍需升级，在线更新不会绕过协议能力检查。

## VPN 专用账号认证

TCP、WSS、UDP 和 QUIC 共用 VPN 账号认证流程。两端仍需匹配的 VPN 共享密钥。服务端在分配虚拟 IP 或处理测试回显前校验用户名和密码证明。每次连接使用新的认证挑战，控制消息中携带密码证明。

在服务端 `sgate-server.cf` 中设置 `vpn-accounts-file = vpn-server-accounts.json`，在客户端 `sgate-client.cf` 中设置 `vpn-credentials-file = vpn-client-credentials.json`。相对路径按各自配置文件所在目录解析。服务端文件可列出多个账号，客户端文件包含一个启用的账号，用户名和密码与服务端条目匹配。两个文件采用以下格式。

```json
{
  "version": 1,
  "accounts": [
    {"username": "alice", "password": "<换成你的 VPN 密码>"}
  ]
}
```

服务端可为某个条目设置 `"disabled": true`。文件以明文保存密码，请限制文件权限，Unix 下可用 `chmod 600`。管理界面配置文件路径，“测试 VPN 连接”读取客户端文件中的凭据。更改文件内容或路径后重启 VPN 加载新配置。

两端管理界面的“VPN 账号”页可直接保存凭据。服务端可新增账号、修改用户名或密码、禁用及删除账号。客户端保存单组凭据，留空密码可保留原值，也可用该页的“测试 VPN 连接”检查已保存的配置和凭据。首次保存且未指定路径时，页面会在配置目录创建本端专用文件并登记路径。保存后，可在配置页重启 VPN 应用账号变更。

填写文件路径即启用账号认证。`vpn-require-account = true` 可以在文件路径误删时阻止 VPN 启动。旧配置默认只使用共享密钥。启用账号认证的两端需支持新版认证。独立 CLI 对应服务端 `accounts-file`、客户端 `credentials-file` 和可选的 `require-account`。

## TCP / WSS 备用传输

Linux 服务端可设置 `vpn-tcp-notsent-lowat`，对新建 TCP/WSS VPN 套接字应用 `TCP_NOTSENT_LOWAT`，减少大下载提前写入内核的未发送字节。默认 `0` 使用系统默认，最大 `1M`，重启 VPN 生效。独立 CLI 参数为 `tcp-notsent-lowat`。外层 TCP 在途数据的顺序和重传仍由 TCP 处理。

两端升级新二进制和管理页面后，在服务端“独立 VPN”配置中增加：

```ini
# Keep QUIC/UDP available.
vpn-listen = 0.0.0.0:8444
# TCP and WSS share this separate TLS listener.
vpn-tcp-listen = 0.0.0.0:8444
```

TCP 与 UDP 可使用相同数字端口；该 TCP 端口不能被代理、管理服务或其他进程占用。放行云端及主机防火墙的 **TCP 8444**；原 QUIC 需要的 UDP 规则继续保留。若仅使用可靠传输，可将 `vpn-listen` 留空并保留 `vpn-tcp-listen`。新监听默认留空，升级不会自动开放 TCP 端口。

客户端在同一 `vpn-server` 字段中选择一种方式：

```ini
vpn-server = tcp://i18n.webcool.cn:8444/vpn
# Alternatively: wss://i18n.webcool.cn:8444/vpn
# Original mode: quic://i18n.webcool.cn:8444/vpn
```

`tcp://` 使用 TLS 1.3 加密并升级为原生 VPN 帧协议，**不是明文 TCP**；`wss://` 在 TLS 上通过标准 WebSocket 二进制消息承载 VPN 帧。两者共用 `/vpn` 路径和同一证书/密钥配置，均要求显式端口，不支持 `ws://`、跳过证书验证或 URL 中携带认证信息。WSS 可经支持 WebSocket Upgrade 的 HTTPS 反向代理转发；使用原域名进行 SNI 和证书校验，不受 HTTP_PROXY 环境变量影响。

先用“测试 VPN 连接”确认所选协议、TLS 与共享密钥认证通过，再保存 `vpn-server`。运行中的 VPN 自动切换并重连；已停止的 VPN 保持停止，需在概览手动启动。切换协议会结束当前 VPN 会话，现有业务连接会断开。协议由用户选择，没有静默自动回退。概览显示实际传输方式；服务端额外显示 TCP/WSS 监听地址。客户端手动启停、网络清理、重连和 VPN 白噪音均适用于新增协议。

可靠传输直接承载完整 IP 包，不沿用 UDP 的 1000 字节分片、两秒重组过期及 MSS 960 限制。外层 TCP 恢复丢失的数据，避免内层 TCP 直接承担每个外层丢包的恢复；仍可能出现 TCP 队头阻塞及内外层 TCP 相互影响，不保证所有网络条件下更快。原 QUIC 握手与数据格式不变，新客户端选择 `quic://` 时可继续连接旧 VPN 服务端；TCP/WSS 要求新版服务端启用监听。新版服务端可同时接入三种传输，虚拟 IP 唯一性和客户端数量限制跨传输统一生效。

独立 `sgate-vpn` CLI 对应服务端 `tcp-listen`（可与 `listen` 同时配置）、客户端 `server` URL；其余参数相同。Linux 隔离测试脚本新增 `--transport quic|tcp|wss`，可分别使用 `--delay-ms 100 --loss-percent 2` 比较同条件弱网数据转发、完整性、启停和清理。此脚本需要 Linux/root/network namespace，macOS 本轮没有实测该特权路径。

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

## macOS 服务端测试

集成模式使用相同的 `sgate-server.cf` 和网页“独立 VPN”分组，设置 `vpn-mode = only` 或 `both`。配置 `vpn-listen`、相同的 `vpn-network` / `vpn-mtu`、证书和密钥；`vpn-egress-interface` 留空按当前到 `1.1.1.1` 的路由识别，也可填写实际物理网卡，例如 `en0`。以 root 启动服务，正常退出使用停止脚本或 SIGTERM。

macOS 自动创建新的 `utunN`，配置网关 `10.77.0.1` 和 VPN 子网路由，再启用 IPv4 forwarding。PF 使用标准 `com.apple/*` 挂点下的唯一 `sgate-vpn-<pid>-<随机值>` 子锚点，仅添加该 VPN 子网到指定物理出口的 NAT 和过滤规则。NAT 后的出站包通过唯一标签匹配，避免放行无关的物理出口流量。退出或中途失败会回滚地址/路由，清理自己的 NAT/filter 规则，释放 `pfctl -E` 返回的引用令牌，并恢复原 forwarding 值。

程序不会重载 `/etc/pf.conf`，不会清空主防火墙或全局状态表，不会直接执行 `pfctl -d`。要求主规则已经加载标准 `nat-anchor "com.apple/*"` 和 `anchor "com.apple/*"` 挂点；缺少时界面会显示错误，需要管理员检查已有 PF 配置，或选择 `vpn-manage-network=false` 自行配置网络。既有自定义防火墙和应用防火墙仍可能要求显式放行 VPN UDP 监听。

全局 forwarding 的修改需要与其他共享网络服务协调，同一主机不要并行运行多个自动管理 forwarding 的 VPN 服务端。SIGKILL、断电等无法保证用户态恢复；如需排查，启动日志会记录本次 PF 锚点，PF 引用可由 `pfctl -s References` 查看；锚点移除后残留的连接状态由内核过期，不清空其他服务的状态。

建议用另一台 Mac/Linux 或具有独立网络的虚拟机作为客户端连接此 Mac 的局域网地址；证书 SAN 须包含该地址或域名，自签名证书配置客户端 `vpn-tls-ca`。随包测试证书只覆盖回环地址，局域网测试应生成匹配证书。同一台 Mac 同时运行服务端和全隧道客户端会共享并冲突系统路由，不作为互联网端到端测试方式。

独立 CLI `sgate-vpn -role server` 也支持 macOS，使用相同自动网络逻辑；其配置项使用 `egress-interface` 等未加 `vpn-` 前缀的名称。

## 中国 macOS 客户端

先停止 OpenVPN 或其他接管全隧道路由的 VPN；程序遇到现存路由冲突会失败并回滚，不覆盖另一 VPN 的路由。

```ini
role = client
server = quic://你的证书域名:8444/vpn
key-file = /opt/soft/sgate/conf/vpn.key
tls-ca = ''
vpn-network = 10.77.0.0/24
# Virtual IPv4 address is assigned by the server after authentication.
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

- 服务端在 `vpn-network` 的可用主机地址中自动分配唯一虚拟 IPv4，跳过网络、网关和广播地址；认证与分配在同一临界区完成，断开后释放。在线重连复用同一身份的现存地址，会话已释放后可能分配不同地址；客户端在安装新地址前先清理旧网络设置。地址池或客户端数达到上限时明确拒绝接入。连通测试不占用地址、不替换在线会话。MTU、子网仍需两端一致。
- 自动分配适用于 QUIC、TCP/TLS 和 WSS。新客户端要求服务端支持地址分配，请先升级服务端；旧服务端会明确提示需要更新。新服务端仍接受旧客户端显式申请空闲地址，方便逐端升级。
- TLS 1.3 验证服务器证书；控制流使用随机挑战和共享密钥 HMAC 进行双向确认。未认证报文不会写入 TUN。不使用 0-RTT 发送 IP 包。
- 当前是一个共享密钥信任域，持有密钥即获 VPN 准入；不是既有 Web 管理账号或 TCP 账号 ACL，也没有单用户撤销功能。
- 服务端校验每个报文的源地址必须等于该连接绑定地址，禁止客户端互访；客户端校验目的地址必须为自己的虚拟地址。
- MTU 576～1500，默认 1280。一个 IP 包拆成最多两个 1000 字节负载片段，适应 QUIC 初始 1200 字节 UDP 负载；不把大 IP 包回退到可靠流。
- 每连接至多 64 个重组项，2 秒过期；发送队列 64 包。丢片只影响对应 IP 包，后续其他包仍可交付。拥塞/离线时丢弃，不建立 IP 重放缓存。
- QUIC 自身仍有拥塞控制，DATAGRAM 并不保证送达、顺序或性能；它消除了此层的可靠流重传等待，不能消除跨境 UDP 丢包和路径限速。
- 客户端保存虚拟 IP，断连后退避重连，不重新应用路由。网络配置失败直接退出并回滚，避免无限重试特权命令。服务器地址在进程启动时解析并固定，域名切换 IP 后需重启客户端。
- VPN 支持独立 DATAGRAM 白噪音，无 WSS 回退，保持 QUIC 心跳。每 10 秒记录 IP 收发字节、拒绝/丢弃、连接数、最近平滑 RTT、QUIC 丢包判定和 PTO 事件。RTT 是最近连接采样，不是业务 ping 延迟；TX 是交给 QUIC 的 IP 负载，不代表对端已收到。

## VPN 白噪音

集成模式在客户端与服务端的“独立 VPN”分组中分别设置，默认关闭：

```ini
vpn-noise-rate = 10K
vpn-noise-traffic = true
vpn-noise-idle = true
vpn-noise-idle-after = 1s
```

`vpn-noise-rate` 是本端每条已认证 VPN 连接的噪音负载字节/秒，支持 K/M（1024 倍率），0 关闭，上限 10M；两端独立设置，只影响各自发送方向。服务端总目标随在线客户端数增加。与原代理的 `noise-*` 配置分开；独立 `sgate-vpn` CLI 使用 `noise-rate`、`noise-traffic`、`noise-idle`、`noise-idle-after`。这些白噪音参数现已支持通过网页保存后立即生效，不重建隧道；命令行覆盖仍优先。

噪音载荷由安全随机数生成，封装为独立 QUIC DATAGRAM。仅在认证并协商白噪音能力后发送；接收方识别后丢弃，不注入 TUN、不交给业务服务、不回显。接收噪音不要求本端配置发送速率。旧端不支持时保留正常 VPN，停止向它发送噪音；概览会提示需要更新两端。连接测试不发送这种噪音。

发送端按 5ms 时钟计算额度，低速时合并为小包以减少包头开销；优先处理待发送的业务 IP 数据，并在 QUIC 数据报排队或拥塞窗口不足时让行。暂停、断线和调度延迟不积攒补发额度，因此实际速率可能低于目标。白噪音仍会消耗实际链路与 QUIC 拥塞预算，目标速率不保证带宽或传输性能。

纯 UDP、TCP 和 WSS 没有 QUIC 发送窗口信号。两端观察本连接收发的大业务 IP 包（达到 1000 字节或 TUN MTU 的四分之三，取较小值），以及耗时达到 10 ms 的发送。遇到这类本地压力信号会暂停填充 1 秒。压力持续会延长暂停；业务发送不等待填充。轻量业务可按原忙闲策略继续填充。该机制是保守的本地判断，无法直接测量远端瓶颈。纯 UDP 的业务包仍只发送一次。

`vpn-noise-traffic` 控制最近任一方向有业务 IP 数据时是否发送；`vpn-noise-idle` 控制双向持续无业务 IP 数据时是否发送，阈值由 `vpn-noise-idle-after` 决定（0 使用 1s）。认证成功且从未传输业务数据的连接视为空闲，不要求存在应用 TCP 连接；无在线 VPN 连接时不发送。心跳与噪音不会刷新业务活动时间。

两端概览显示本端生效速率、忙闲策略、噪音累计收发字节及旧端兼容提示。带宽图选择“VPN 白噪音”，查看全部 VPN 客户端的噪音负载速率；“VPN IP”仍只统计 IP 业务负载。两类计数均不包含 UDP/QUIC/IP 封装开销，发送计数表示提交给 QUIC，不保证对端收到；丢弃/拥塞让行不会形成恢复后的突发发送。

## 验证状态

VPN 白噪音另验证双向/单向收发、不会写入 TUN、业务传输共存、速率及暂停额度、旧端兼容和能力认证绑定；弱网实际达到的速率仍待跨境验证。

本机使用内存 TUN 和真实 UDP/QUIC 套接字验证了双向 IPv4 包（包含 1280 字节分片）、重连、身份与证书拒绝、地址冲突、源地址伪造/私网隔离、丢片与乱序独立交付及内存边界。集成模式另验证旧配置的网页升级、输入校验、仅 VPN 的代理拒绝、运行错误展示及内存 TUN 下的 VPN 监听/退出；`python3 scripts/test-vpn-management.py` 使用真实客户端进程与认证 API 验证保存待应用、应用/重试/停用、缺失的未使用映射文件和权限/设备错误展示，刻意使用无效设备名，不创建 TUN。macOS 服务端网络使用命令替身验证 PF 独立锚点、引用令牌释放、forwarding 恢复、路由冲突、加载失败回滚及不操作共享规则；生成规则通过系统 `pfctl -n` 的实际语法检查（不加载规则、不启用 PF）。macOS 客户端路由/DNS 使用命令替身验证创建、失败回滚和恢复，不会改动当前 OpenVPN 或系统网络。

**尚未执行 macOS 特权 utun/路由/DNS/PF NAT 端到端实测，以及 Linux 内核 TUN/NAT/实际互联网端到端实测。** Linux 代码的交叉编译不代表运行验证。按此前约定，没有 Linux 环境时交付测试脚本并明确标记未实测。

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

手动控制仅适用于集成的 `sgate-client`，独立 `sgate-vpn client` 命令仍直接启动 VPN。正常退出（SIGTERM/SIGINT）也执行网络清理；强制终止（SIGKILL）或断电时，集成客户端会在下次启动后提示是否清理已记录的遗留网络设置。请在实时概览确认清理，然后手动启动 VPN。恢复记录跟随客户端配置文件，只覆盖本版本记录的服务器物理出口路由和 macOS 物理网络服务 DNS。`vpn-manage-network=false` 时系统网络由外部管理，sgate 不负责清理外部路由。

### 负载下延迟与发送调度

VPN 每个客户端仍使用一条 QUIC 连接，IP 数据通过 DATAGRAM 传输。发送端按内层 IPv4 源/目标地址、协议及 TCP/UDP 端口分流，采用 1500 字节量子的 DRR 公平调度；ICMP 回显按标识符区分，IP 分片按地址及协议归入同一流。每连接总缓存仍最多 64 个 IP 包；满时从最长流淘汰最旧包，让新短流有机会入队。默认不按等待时间丢弃包（`vpn-queue-max-age=0s`）；可选配置最多 1m 的过期时间。此前固定 250 ms 的策略会在高 RTT、低拥塞窗口下频繁丢弃正常 TCP 包，已取消默认启用。这是有界公平队列加可选驻留期限，并非完整 FQ-CoDel。

QUIC 待发送数据报达到 4 个后暂停从公平队列取包，等发送回调通知再继续；当前每个 IP 包最多分为 2 个数据报，因此 QUIC 内部业务/噪音积压最多约 5 个。这仅约束尚未交给 UDP 的数据报，不限制正常在途数据或改变 QUIC 拥塞控制。协议格式和认证不变，可逐端更新；下载应优先更新发送数据的服务端，双向优化需更新两端。

实时概览新增 QUIC 最近采样 RTT、拥塞窗口/在途字节数，以及发送队列峰值、已交付包最长排队、过期/满队列丢弃、QUIC 提交最长耗时、QUIC 准入等待累计及 TUN 交付最长耗时。队列和耗时峰值/计数自 VPN worker 启动累计，QUIC RTT/拥塞窗口/在途字节是最近回调采样，服务端多连接时来自最近更新的连接，不表示全体连接总量；服务端累计等待覆盖所有客户端，并非当前延迟，也不是公网 RTT。TUN 交付计时包含校验、服务端写锁等待和设备写入。指标用于区分程序内排队、QUIC 发送受阻和本机交付阻塞；外部 UDP 路径拥塞或路由器缓冲仍可能造成额外延迟。主动丢弃可能增加内层 TCP 重传，吞吐和延迟需一起测量。

使用相同网页加载/下载任务，比较修改前后的空闲及负载下 ping P50/P95/P99、丢包和业务吞吐，并观察两端上述指标。此优化已通过本机调度、拥塞准入和内存 TUN/真实 QUIC 回归；中美公网与 Linux netem 的实际性能收益尚未验证。

### 大包、TCP MSS 与丢片恢复

默认 MTU 1280 下，大于 1000 字节的 IP 包在当前协议中会拆为两个 QUIC DATAGRAM。任一分片丢失都会使整个 IP 包无法交付。TCP SYN/SYN-ACK 现在双向检查 MSS，只将过大的 MSS 下调到 `min(vpn-mtu, 1000) - 40`（默认 960），并正确更新 TCP 校验和，使普通 TCP 数据尽量使用单个数据报。RFC 6691 要求发送端另外扣除实际携带的 IP/TCP 选项长度。已经较小的 MSS、非 SYN、IP 分片、无 MSS、含 TCP MD5/AO 或格式异常的选项不会改写；不增加或插入 MSS。该策略不会限制 UDP/HTTP3 的包长，较大的 UDP 包仍使用分片。

此前最多 64 项的重组表满时会拒绝新分片，旧缺片记录持续到两秒过期；持续丢包可能令新完整包也无法进入。现在容量满时淘汰最旧的未完成记录，允许新包立即参与重组，保持 64 项上限和逐包独立交付。概览及 stats API 新增 `tcp_mss_clamped_packets`、`fragment_reassembly_expired_packets`、`fragment_reassembly_evicted_packets`；重组超时和淘汰均计入总 IP 丢弃。过期记录在后续分片到达时回收，计数不是公网丢包率。

这些变化不修改线协议，可与旧端逐端升级，建议更新两端后重启 VPN。MSS 只影响新的 TCP 握手，测试前关闭并重新打开浏览器以建立新连接。MSS 变小可能增加内层 TCP 包数及头部开销，需要同时比较弱网可用性、吞吐与 CPU。本机验证覆盖 TCP 校验和、奇偶对齐及 IP/TCP 选项、分片表满时恢复和真实 QUIC 双向握手包传递；没有重现用户实际 Gmail 登录会话，也未验证中美公网收益。

### 不依赖聊天连接的 Gmail 对照诊断

切换到故障 VPN 可能同时中断远程协助或聊天连接。使用本地 `scripts/diagnose-vpn.py` 完成测试并保存报告，再恢复可用网络；无需在故障 VPN 下等待在线回复。脚本不启停 VPN、不改路由/DNS，不读取浏览器 cookies、账号凭据或邮件；只测试未登录的公共 HTTPS 入口，响应正文和头部均丢弃。报告默认私有权限且不覆盖已有文件，每个探测后保存进度；被中断时可读取已完成部分。

先在 OpenVPN 下运行：

```sh
python3 scripts/diagnose-vpn.py --label openvpn --output /tmp/gmail-openvpn.json --timeout 8
```

然后手动停止 OpenVPN、启动 sgate VPN，在终端运行：

```sh
python3 scripts/diagnose-vpn.py --label sgate --output /tmp/gmail-sgate.json \
  --reference /tmp/gmail-openvpn.json --timeout 8 --budget 90
```

输出 `Saved` 后停止 sgate、恢复 OpenVPN。`--reference` 同时测试上一份报告的固定 IPv4 地址，保留 HTTPS 主机名及证书校验，用于区分系统 DNS 差异与传输差异。脚本包含相关域名的 macOS DNS 覆盖规则、系统 DNS 结果、VPN 端点及默认互联网路由、HTTP/1.1 与可用时的 HTTP/2/HTTP/3。系统 curl 不支持 HTTP/3 时明确跳过，不能据此断言 HTTP/3 可用或失败；`--http2` 的实际协商结果见 `http_version`。预算耗尽或测试期间路由变化会标记报告，未执行的探测不视为网络失败。

HEAD 的 302/404 仅证明对应端点的 HTTPS 已建立，不证明登录后的 Gmail、浏览器代理、JavaScript、HTTP/3 或全部资源可用。`--download` 可额外采样公开 Gmail 登录入口的有限下载，不登录账号；超时后有 HTTP 200 和已下载字节也不能视为完整页面成功。建议保持两次测试配置相同、白噪音关闭，报告保留原始错误码及各阶段耗时，而非仅比较 ping。

若 HEAD 均通过但 Gmail 仍停在加载页，使用正文模式（本地独立运行，完成后恢复可用 VPN 再查看报告）：

```sh
python3 scripts/diagnose-vpn.py --label sgate --output /tmp/gmail-sgate-body.json \
  --download-only --timeout 20 --budget 60
```

正文模式分别尝试 HTTP/1.1 和本机 curl 支持时的 HTTP/2，跟随公开入口重定向；记录 `size_download`、`transfer_completed` 及 `stage`，区分未建立连接、TLS 阶段、已收到部分正文但超时。公共入口测试仍不能代替登录后的 Gmail 资源请求；浏览器 Network 中关键脚本/Fetch 的域名、错误和 h2/h3 协议有助于定位，常驻长轮询 Pending 本身并不表示故障。不需要提供 cookies、Authorization 或带查询参数的完整 URL。

### 使用客户端配置进行隔离的真实 VPN HTTPS 测试

新版 `sgate-client -vpn-diagnose` 在启动框架/代理/管理界面之前进入一次性诊断，读取现有 `.cf` 的 VPN 服务端、密钥、CA、子网、虚拟 IP 和 MTU。先停止 sgate VPN（OpenVPN 可以保持连接），使用新版程序在管理员终端运行：

```sh
/绝对路径/sgate/bin/sgate-client -alone \
  -f /opt/soft/sgate/conf/sgate-client.cf -vpn-diagnose \
  > /tmp/sgate-vpn-isolated.json
```

只有配置关闭 VPN 时，才在上述命令另加 `-vpn-mode=only` 以诊断配置中已填写的 VPN 参数；不会保存这个覆盖。默认使用原配置文件路径；如果密钥/CA 路径为相对路径，须在正常客户端启动时相同的工作目录运行。既有 `master_args` 中通过命令行提供的覆盖需要在此命令中同样提供。

诊断先执行不占用地址的认证/回显测试；已有同虚拟 IP 会话或客户端数达到上限时直接返回，不挤掉现有连接。随后独立创建 TUN，只为 mail.google.com、accounts.google.com、www.gstatic.com、ssl.gstatic.com 的解析 IPv4 地址添加临时 `/32` 路由；不修改默认路由、DNS、IPv6 路由或配置文件，诊断白噪音关闭。已有目标主机路由冲突时失败并回滚，不覆盖别人的路由。使用系统 TCP/TLS 和 HTTP/2（可协商时）通过真实 sgate 数据报进行公开 GET 下载，报告请求状态、字节数、耗时、错误及 VPN 指标。请求不使用浏览器代理、cookies、账号或邮箱数据，最多读取每个响应 2 MiB，单项 HTTPS 上限 45 秒，整个诊断限时 180 秒。

正常完成、超时或 SIGINT/SIGTERM 时等待测试客户端退出并清理其临时网络，再输出 JSON。不要用 SIGKILL。此测试能验证真实 VPN 的 IP/TCP/TLS/公开正文传输，仍不等于登录后 Gmail 的所有资源或浏览器 HTTP/3 均通过。诊断代码已通过网络范围和占用地址保护测试；真实特权 TUN/路由步骤需要在用户机器的管理员终端验证。

诊断 TCP 套接字同时绑定测试 TUN 网卡与虚拟源 IPv4，返回 `local_address` / `remote_address`，避免系统分流让测试流量绕过 sgate。每项记录首次响应时间、该时间段的 VPN IP 收发字节、`body_was_gzip_decoded`；Go 自动 gzip 解压后的正文大小与隧道 IP 字节不能直接比较。HTTP 200 后读正文超时表示响应未在测试期限内完成，不等同于认证失败或 Gmail 不可达；这类报告仍保留部分字节和具体错误。客户端零丢包计数不排除服务端发送方向或中间网络丢包。

服务端实际排查曾观察到累计 1877 个过期丢弃、28 个满队列丢弃和约 3.43 KiB 拥塞窗口。这些不是同一请求的精确因果统计，但显示固定 250 ms 期限会在该链路大量主动丢弃。现在两端默认关闭时间过期，保留每连接 64 包上限、按流 DRR、公平入队及 QUIC 准入限制；满队列时仍从最长流淘汰最旧包。`vpn-queue-max-age` 可在两端配置及网页独立设置；独立 CLI 对应 `queue-max-age`。升级旧配置缺少此项时默认 0，不需要先补配置才能修正。启用过期策略仍可能牺牲 TCP 有效吞吐，不能仅凭 ping RTT 下降判定改善。

### 无 root、无系统路由变更的用户态隔离测试

`tools/vpn-diagnose` 是独立 Go 模块，使用 WireGuard 项目提供的用户态 TCP/IP 栈，将生成的 IP 包交给现有 `vpn.RunClient`；实际隧道按 VPN 地址选择 sgate QUIC、TLS/TCP 或 WSS。它不创建系统 TUN、不调用网络配置函数，也不需要停止当前 OpenVPN。额外的 netstack 依赖只在此工具模块中，不进入正式客户端/服务端依赖。配置、共享密钥和 CA 文件须对当前用户可读；程序不输出密钥，不使用浏览器 cookies。已有同虚拟 IP 会话时拒绝运行，不抢占在线 sgate 客户端。

```sh
go -C tools/vpn-diagnose build -mod=mod -o /tmp/sgate-userspace-diagnose .
/tmp/sgate-userspace-diagnose \
  -config /opt/soft/sgate/conf/sgate-client.cf \
  -output /tmp/sgate-userspace-system.json -dns-source system -http h2
```

`-dns-source vpn` 使用配置中的 VPN DNS，通过用户态隧道解析测试目标，帮助对比本机 `/etc/resolver` 规则的影响；`system` 使用系统 DNS 后将所选 IPv4 固定给测试连接。`-http h1|h2|h3` 分别控制 HTTP 版本，h3 强制通过用户态 UDP/QUIC 且不回退。`-only mail.google.com` 可只测试一个允许的主机；`-reference <旧报告>` 固定旧报告的 `target_ips`，避免两次 DNS 不同。内层 H3 固定使用 1200 字节 QUIC 初始包且关闭路径 MTU 扩展，以减少对照变量。仍会经过当前 VPN 的 IP 分片机制。

报告记录每个请求的虚拟源地址、目标地址、协议、状态、首次响应、完整正文/采样上限、gzip 解压情况及该时间窗口的 VPN IP 字节。包观察仅记录 TCP/UDP/ICMP 计数及有限的 ICMP 错误类型、代码、MTU 和引用目标，不记录数据包内容。`-assets` 尝试从公开登录 HTML 提取最多 6 个允许域名的外部 script/stylesheet；`sampled_assets=0` 时没有验证外部脚本，不能将公开登录页通过视为登录后 Gmail 应用通过。

2026-11-03 的实际测试：系统 DNS 下公开 Gmail/Accounts 正文约 953 KB，HTTP/2 分别在约 12.7/10.6 秒完整接收；VPN 内 DNS 下分别约 19.1/21.1 秒。两组客户端队列过期、满队列和总丢弃均为 0。顺序测量不足以断言某个 DNS 更快。强制 H3 时 Accounts 可完整接收，选中的 Gmail IP `108.177.125.18` 握手失败；相同 IP 在 OpenVPN 的本机路由对照中也出现远端拒绝，因此不能把该 H3 结果单独归因为 sgate。用户态单目标复测未收到 ICMP MTU 错误。未使用登录态、未执行浏览器 JavaScript、未验证真实系统 TUN/DNS 接管后的浏览器行为。

#### 浏览器真实资源隔离测试

工具的 `-browser-proxy` 模式可让浏览器沿用登录态，通过同一真实 VPN 会话测试 Gmail；TLS 仍由浏览器与 Google 端到端校验，工具不解密或记录 Cookie、URL 参数、HTTP 正文或邮件。仅监听 `127.0.0.1`，只允许 Google/Gstatic/Googleusercontent/Googleapis 域名的 HTTPS CONNECT，并限制并发和报告条目数。

```sh
/tmp/sgate-userspace-diagnose \
  -config /opt/soft/sgate/conf/sgate-client.cf \
  -output /tmp/sgate-browser.json -browser-proxy -dns-source system -budget 12m
```

启动后显示临时 PAC URL。先记录浏览器原代理设置，再暂时使用该 PAC；仅相关 Google 域名经过隔离会话，其余域名直连。结束测试必须恢复浏览器原代理设置，再用 SIGINT 停止工具。工具不自动修改或恢复浏览器设置。`-proxy-listen 127.0.0.1:端口` 可指定回环端口；默认随机分配。代理采用总测试预算，不对每个隧道另设短截止时间，避免把慢请求人工截断后误诊为产品故障。`-dns-source` 同时适用于代理，每个域名固定首次选中的公开 IPv4。`-asset-url` 可单独 GET 已观察到的公开 `mail-static` 资源，拒绝账号凭据、查询参数和范围外目标。

2026-11-03 浏览器复测真实邮箱加载时，主脚本及样式请求失败，约 4.22 分钟后 Gmail 显示暂时性错误，帮助链接带 `error=1010`，技术数字代码为 4。Service Worker 的错误只表示资源请求失败，不能单独证明缓存损坏。一个实际公开主脚本共 1,394,702 字节：经 sgate、目标 `142.251.41.5` 完整传输约 98.45 秒；经同一美国服务器的临时 SSH/TCP 通道并固定同一 Google IP，约 6.71 秒完整传输。系统 DNS 的另一目标 `108.177.125.83` 经 sgate 在 120 秒收到 1,115,194 字节后超时。短请求/公开登录页通过不足以代表此场景通过。

单脚本采样约 42 秒内，服务端 QUIC 丢包判定从 5194 增至 5297，应用队列满丢弃保持 1106、过期丢弃保持 0，拥塞窗口采样约 4.5–6.1 KiB。服务器 CPU 约 97% 空闲；另外 20 秒内核采样中 UDP 错误和 eth0 qdisc 丢包未增长，8444 套接字收发缓冲各 14 MiB、套接字丢弃为 0。历史累计丢包不是本次请求的丢包率。服务器 TCP 算法为 BBR；所用 quic-go v0.54.0 的发送实现选择 Reno，不能通过 Linux `tcp_congestion_control` 设置改变 QUIC 算法。

这些证据将瓶颈缩小到 sgate 下载传输方向；高丢包/乱序下外层 QUIC 与内层 TCP 的拥塞响应是有依据的候选原因，但尚未区分运营商 UDP 限制、真实链路丢包、乱序以及用户态 TCP 栈差异，也未量化算法差异的贡献。上述诊断阶段增加的是诊断能力和既有 QUIC 丢包/PTO 计数显示，当时尚未实现 TCP/WSS 备用传输，也没有将传输瓶颈标记为已修复。首轮代理曾设置两分钟截止时间，其浏览器部分传输错误已排除，不能作为产品自然断流的证据。

隔离诊断工具可用 `-server tcp://域名:8444/vpn` 或 `-server wss://域名:8444/vpn` 临时覆盖现有客户端的 VPN 地址，不保存配置，便于在服务端升级后对照 Gmail 的真实资源；其余密钥、CA、子网和 MTU 继续取自原配置。

新增传输的本机验证已通过：主项目竞态回归、TCP/WSS/QUIC 混合接入、认证与证书拒绝、重连、白噪音、异常帧和停机清理；另以两端用户态 TCP/IP 栈通过三种传输完整下载约 2 MiB HTTP 资源并完成 64 KiB 上传回显。可运行 `go -C tools/vpn-diagnose test -mod=mod -race ./...` 复测，该测试不创建系统 TUN 或修改路由。已生成 macOS arm64 与 Linux amd64 二进制；尚未部署美国服务器，也未用新 TCP/WSS 模式验收真实 Gmail。
