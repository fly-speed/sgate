# sgate

**加密 TCP 端口代理 · 独立 IPv4 VPN**

当前版本 `1.5.0` · [English documentation](README.en.md)

sgate 用两个进程连接本地与远端网络。只需访问 SSH、数据库或内网 Web 服务时，映射所需 TCP 端口即可。希望应用按系统路由访问远端网络时，可以启用独立 VPN。两种模式可同时运行，并可通过 Web 管理界面配置和观察。

![sgate 的 TCP 端口代理和 IPv4 VPN 数据路径](docs/images/sgate-paths.svg)

## 先选使用场景

| 你想做什么 | 选择 | 应用如何接入 |
| --- | --- | --- |
| 从本机访问远端 SSH、数据库或内网 Web | **TCP 端口代理** | 连接本机映射端口。客户端可配置多条映射 |
| 让服务端一侧访问客户端内网的 TCP 服务 | **反向 TCP 代理** | 服务端监听反向端口，由已授权客户端连接内网目标 |
| 让浏览器等应用按系统路由走 VPN | **独立 IPv4 VPN** | 应用照常运行，系统把匹配路由的 IP 包交给虚拟网卡 |

客户端支持 **macOS、Linux、Windows**。服务端支持 **macOS、Linux**。Windows 包另附独立控制 APP，可启动和停止客户端进程，并打开 Web 管理界面。开启 VPN 需要虚拟网卡和网络管理权限。 [Windows 客户端指南](docs/vpn.md#windows-客户端)

## 快速安装

**macOS / Ubuntu** 选择对应架构的 `sgate-1.5.0-<系统>-<架构>.tar.gz`。Apple Silicon 用 `arm64`，Intel Mac 和常见 x86_64 Ubuntu 用 `amd64`。解压后运行包内的安装脚本。以下用 Apple Silicon 和用户目录举例。

```sh
shasum -a 256 -c sgate-1.5.0-macos-arm64.tar.gz.sha256  # Ubuntu 可用 sha256sum -c
tar -xzf sgate-1.5.0-macos-arm64.tar.gz
cd sgate-1.5.0-macos-arm64
PREFIX="$HOME/.local/sgate" ./install.sh
```

系统级安装可运行 `sudo ./install.sh`，默认目录为 `/opt/soft/sgate`。安装包包含两端程序、配置模板和 Web 文件。安装会保留已有配置和密钥，且不启动服务。

**Windows** 解压客户端包，编辑 `conf/sgate-client.cf`，再用 `bin/sgate-control.exe` 启动客户端并打开管理页。首次配置需将服务端使用的共享密钥安全地复制到客户端。 [Windows 构建和部署](docs/vpn.md#windows-客户端)

**从源码构建** 需要 Go 1.24 或更新版本。运行 `make build`，再用 `sudo make install` 安装；跨平台打包可运行 `./package/package-release.sh ubuntu amd64` 或 `./package/package-release.sh macos arm64`。Windows 源码打包入口是 `package\build-windows.bat`。运行 `sgate-client -version` 或 `sgate-server -verbose` 可查看版本及构建信息。

## 第一次跑通代理

以下用同一台 macOS 或 Ubuntu 机器验证。默认模板让客户端的 `127.0.0.1:9000` 经服务端的 `127.0.0.1:8081` 访问后端 `127.0.0.1:8080`。

1. 在安装目录的 `conf/sgate-client.cf` 和 `conf/sgate-server.cf` 中，把示例 `password` 改成相同的自选密码。同一次安装已生成两端共用的 `conf/sgate.key`。
2. 分别打开三个终端，运行下面的三条服务命令。若用的是系统级安装，将 `$HOME/.local/sgate` 换成实际安装目录，并以有权读取配置的用户运行。

```sh
# 终端 1：启动仅供测试的 HTTP 后端
demo=$(mktemp -d)
printf 'sgate OK\n' > "$demo/index.html"
python3 -m http.server 8080 --bind 127.0.0.1 --directory "$demo"

# 终端 2：启动 sgate 服务端
"$HOME/.local/sgate/sbin/sgate-server" -alone -f "$HOME/.local/sgate/conf/sgate-server.cf"

# 终端 3：启动 sgate 客户端
"$HOME/.local/sgate/sbin/sgate-client" -alone -f "$HOME/.local/sgate/conf/sgate-client.cf"

# 另开终端验证，应看到 sgate OK
curl --noproxy '*' http://127.0.0.1:9000/
```

3. 打开客户端管理页 `http://127.0.0.1:9081/` 和服务端管理页 `http://127.0.0.1:8081/admin/`。首次登录按提示读取各自 `admin_setup_token_file` 指定的文件，并创建独立的 Web 管理员账号。

部署到两台机器时，客户端在「代理配置 → 代理映射」填写服务端地址，服务端填写后端目标，并只开放所需的监听。两端使用相同的共享密钥及匹配的代理凭据。若映射单独指定后端，将它加入服务端 `allowed-targets`。跨公网使用 WSS 或 QUIC 时，配置证书并校验服务端名称。 [部署参考与反向代理](docs/reference.md) · [配置示例](samples/README.md)

## 启用独立 VPN

1. **服务端** 在「VPN 配置 → VPN 参数」把 `vpn-mode` 设为 `both`（代理与 VPN 共存）或 `only`（仅 VPN），选择监听地址并应用。TCP 和 WSS 共用 `vpn-tcp-listen`，QUIC 使用 `vpn-listen`，纯 UDP 使用 `vpn-udp-listen`。
2. **客户端** 选择对应模式，填写 `vpn-server`，例如 `quic://vpn.example.com:8444/vpn`。两端的 VPN 子网、MTU 和共享密钥须匹配。使用私有 CA 时在客户端配置信任。服务端在认证后自动分配虚拟 IPv4 地址。
3. 修改 URL 后，可以直接点保存图标右侧的「测试 VPN 连接」。测试使用当前表单，检查连接、认证和回显，无需保存，也不会接管系统路由。
4. 保存并应用后，在客户端「实时概览」手动启动 VPN。进程启动或重启后仍需手动启动。停用时点击「停止 VPN」恢复网络设置。发现上次留下的恢复记录时，页面会提示先确认清理。

需要按用户授权时，两端在「VPN 账号」页配置专用凭据。服务端可新增、修改、禁用和删除账号。代理账号、VPN 账号与 Web 管理员账号分别管理。 [VPN 完整部署、权限与路由说明](docs/vpn.md)

![客户端 VPN 参数页。VPN 地址右侧是保存和连接测试图标](docs/images/vpn-settings.png)

## 为什么这样设计

**代理按连接转发，VPN 按 IP 包转发。** 端口代理只转发选定的 TCP 服务，无需改变系统路由。VPN 通过虚拟网卡接管配置的 IPv4 路由，适合多个应用共用一条隧道。两者使用不同入口，代理 URL 通常是 `/tunnel`，VPN URL 使用 `/vpn`。

**代理断线恢复有明确边界。** WS/WSS 用一条连接复用多路 TCP。原生 QUIC 则为每个业务连接分配独立流，并分别维护确认、队列和重放状态，减少慢连接拖住其他连接的机会。短时断线可在保留窗口内恢复，缓存和流数上限用于控制资源占用。 [代理架构和协议](docs/architecture.md) · [QUIC 性能基线](docs/quic-performance.md)

**VPN 允许按链路选择外层传输。** 四种方式都加密并支持 VPN 专用账号。实际选择应看目标网络的连通性、负载下延迟和吞吐。

| VPN 方式 | 数据怎样传 | 何时考虑 |
| --- | --- | --- |
| `quic://` | QUIC DATAGRAM 承载 IP 包，使用 QUIC 拥塞控制，IP 数据报不重传 | UDP 路径稳定，希望使用 QUIC 拥塞控制 |
| `udp://` | DTLS 加密。握手及部分控制包可重试，业务包不由隧道重传 | 希望减少外层重传，由内层协议自行处理丢包 |
| `tcp://` | TLS/TCP 可靠传输 | UDP 受限，需要直接走 TCP |
| `wss://` | TLS 上的 WebSocket 可靠传输 | 部署入口需要标准 WebSocket |

同一个连接的 TCP/WSS 可靠传输会按序交付，弱网时丢包可能拖延后续数据。QUIC DATAGRAM 和纯 UDP 允许业务数据丢失，应用表现取决于内层协议和链路。纯 UDP 仅用于独立 VPN，TCP 端口映射的应用入口仍然是 TCP。 [纯 UDP 协议与测试](docs/vpn-udp.md)

## 继续阅读

- [部署与测试参考](docs/reference.md) · 参数详解、TLS、acl_master、反向代理、白噪音和历史回归记录
- [实时监控与性能](docs/quic-performance.md) · QUIC 指标、测试口径与弱网基线
- [配置示例](samples/README.md) · SSH、Web、账号、多端口映射与测试证书

许可证见 [LICENSE](LICENSE)。
