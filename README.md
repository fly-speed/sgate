# sgate

Go 编写的通用 TCP 加密通道。客户端和服务端使用标准 HTTP Upgrade 建立一条持久 WebSocket 连接，在其中加密、复用多个双向 TCP 流。应用只需要连接本地 TCP 端口，无需修改协议。

```text
应用客户端 -- TCP --> sgate-client == 加密 WebSocket ==> sgate-server -- TCP --> 应用服务器
                                   <== 双向传输 ==
```

## 功能

- 两个独立程序：`sgate-client`、`sgate-server`。
- 一条 WebSocket 承载多路 TCP 连接，每路对应一个独立的目标 TCP 连接。
- AES-256-GCM 加密应用数据和通道控制信息，检测篡改；预共享密钥通过 HMAC-SHA256 完成双向鉴权。
- 每次连接使用新的随机数，派生不同的上下行密钥；握手限制时间窗口并拒绝随机数重放。
- Ping/Pong 心跳、带抖动的指数退避重连。
- 序号、累计确认、重传及去重，支持 TCP 半关闭。
- 每个会话、每个方向的未确认数据默认限制为 4 MiB；缓存满时停止读取应用数据，通过 TCP 背压限制内存增长。
- 默认每个会话最多 128 路 TCP 流，服务端最多保留 128 个会话。
- 普通 `GET /` 返回 HTTP 页面；未通过鉴权的通道访问返回 404。
- 支持 `ws://`、`wss://`、TLS 自定义 CA、HTTP 反向代理及正常退出。

## acl_master 托管

客户端接入 [go-service](https://github.com/acl-dev/go-service) 的 TCP 描述符接口；服务端按 [gin-service](https://github.com/acl-dev/gin-service) 示例统一使用 `Init → 注册路由 → Run`，独立启动和 acl_master 托管均经过这一生命周期。服务端不再有直接调用 `net/http.ListenAndServe` 的独立分支。两端识别框架的 `-f`、`-n`、`-t`、`-s` 等参数，继承 FD 5 状态通道及 FD 6 起的监听套接字；服务端支持多个监听、HTTPS/WSS，证书继续由 `tls-cert`、`tls-key` 指定。

服务端入口 `cmd/sgate-server/main.go` 直接执行 `gin-service.Init → setRoute → Run`，不再经过工厂回调。`routes.go` 集中列出 WebSocket、管理页面、登录、配置、统计与服务重启入口；`options.go` 仅定义启动参数，`service.go` 仅适配共用的生命周期接口。`internal/app/server.go` 管理业务配置和资源，`sharedServer` 只负责切换当前代理及证书。隧道路径热更新通过兜底路由交给当前代理校验，避免运行中修改 Gin 路由树。

服务端使用本地扩展 [third_party/gin-service](third_party/gin-service/README.md)，以你提供的 `vendor/gin-service` 源码为基础；原参考仓库保持不变。根 `go.mod` 的 replace 选择本地扩展，标准 vendor 中的对应包由 `python3 scripts/sync-go-vendor.py` 同步生成，普通 Go 构建可直接使用。框架自身拥有监听、TLS/HTTP Serve、停止通知、连接计数、Accept/Close 回调和停止等待。WebSocket 升级后仍计为活动连接，实际关闭时才递减；关闭时先断开物理连接，再清理协议会话，避免 TLS 或阻塞写卡住退出。

进程只执行一次 gin-service `Init/Run`。服务端启用管理界面时，管理入口 `/admin/` 与 WSS 共用框架监听；Web 配置应用只替换业务代理并关闭旧会话，HTTP Worker、登录和累计统计保留。已建立的隧道始终属于原代理实例。框架扩展采用每实例可取消生命周期，替代上游全局 Wait/Stop，以支持 TLS、WebSocket 清理及代理热重启。客户端不引入 Gin。

Linux 运行验证可使用 `python3 scripts/test-linux-integration.py --output artifacts/linux-integration`。需要 Linux、本机 Go/C 编译器、Python、OpenSSL、ps、lsof 及可执行的 acl_master/master_ctl（路径不同时通过 `--master`、`--ctl` 指定）。脚本分别构建普通版和竞态版，运行 16 组独立/托管、WS/WSS、相对/绝对路径部署测试，以及 8 组普通/竞态、独立/托管、WS/WSS 压力测试。压力阶段开启随机白噪音、128 并发、频繁断线、默认 6 轮连续重启，并叠加并发登录、配置保存与应用，检查配置保存、代理应用、登录持久化及退出资源。仅使用临时配置和测试端口，不加载已部署服务；输出目录必须为空。压力阶段强制检查客户端 `tcp_framework=go-service`，每次重启后重新核验，并将空闲时框架连接数归零的采样写入报告；缺少采样或框架身份不符时整轮失败。`verification.json` 记录运行平台、命令和结果，跨平台构建不计作 Linux 实测。

与两个模板示例一样，手工启动可显式加 `-alone`，客户端和服务端使用相同方式：

```sh
./bin/sgate-client -alone -f sgate-client.cf
./bin/sgate-server -alone -f sgate-server.cf
```

原有不带 `-alone` 的直接启动命令继续可用；也可以不提供 `-config`，全部通过命令行指定应用参数。需要 Web 管理时加 `-admin-enabled`。在 acl_master 下启动时，`master_args` 可以留空（或仅包含应用参数覆盖项），**不要加 `-alone`**；框架自动传入 `-f <当前 .cf>`，程序直接读取这份文件中的代理与管理配置，无需另一个 `-config`。独立启动使用 `-alone -f sgate-client.cf` 或 `-alone -f sgate-server.cf`。

正式配置仅保留根目录的 `sgate-client.cf`、`sgate-server.cf`，代理与管理参数位于各自的 `service` 块中。客户端通过 `proxies` 合并多个本地代理端口，服务端通过后端允许列表及 `/tunnel?addr=host:port` 选择不同目标；托管模式还需让 `master_service` 与实际监听对应。原 SSH、VPN、Web 专用配置和 SSH 启停脚本已移到 [samples/](samples/README.md)。两份正式配置默认 `master_disable = yes`，管理界面仍默认关闭，部署时按需启用。

托管配置须设置：

```ini
master_type = sock
master_maxproc = 1
master_prefork = 1
master_reuseport = no
master_status_notify = 0
master_unpriv = n
master_chroot = n
master_stop_kill = false
master_stop_wait = true
```

会话和重放状态属于单个进程，因此限制为一个工作进程；不支持把同一个隧道会话分配给多个独立工作进程。监听端口由 `master_service` 决定，应用 `listen` 和 `-listen` 不会重新绑定或覆盖继承端口；其它应用参数仍按“命令行高于配置文件”处理。工作目录由 `.cf` 的 `master_home` 决定，业务相对路径按这个目录解析，管理配置的存储、静态目录及证书路径继续相对管理配置文件解析。运行身份由 acl_master/服务账户设置；模板内再次降权和 chroot 模式会明确拒绝。

启用 Web 管理后，可以继续校验、保存并重启代理，PID 和实际监听端口保持不变，登录会话及本进程累计带宽保持有效。界面会显示监听由 acl_master 管理；若应用新配置尝试修改有效 `listen`，会拒绝并还原本次保存。更换监听地址请修改 `.cf` 的 `master_service`，再通过 `master_ctl` 停止并重新启动该服务。框架停止、重启和 Web 代理重启都会中断当前业务连接，恢复后可以建立新连接。

从旧的 `master_type = none` 配置迁移时，先用旧配置停止服务，再安装新程序、应用配置和 `.cf`，最后启动，避免旧进程占用新框架要监听的端口。例如：

```sh
/opt/soft/acl-master/bin/master_ctl -f /opt/soft/sgate/conf/sgate-client.cf -a stop
# 安装 sbin/sgate-client、conf/sgate-client.cf 及 web/ 文件
/opt/soft/acl-master/bin/master_ctl -f /opt/soft/sgate/conf/sgate-client.cf -a start
/opt/soft/acl-master/bin/master_ctl -a list
```

隔离框架集成测试（需要已安装的 acl_master 和 master_ctl）：

```sh
make build
python3 scripts/test-acl-master.py --merged --client bin/sgate-client --server bin/sgate-server --output artifacts/acl-master-test
```

该测试创建独立临时框架、Unix 管理套接字和随机回环端口，覆盖 WS/WSS、HTTP/HTTPS 管理、每端两个监听、32 路完整性校验、Web 应用、登录持久化和 `master_ctl` 启停/重启。不会载入现有 `/opt/soft/acl-master/conf/service`；`--master`、`--ctl` 可指定其它安装路径，`--client`、`--server` 可指向竞态构建。

托管压力测试需使用带诊断采样的临时构建，采样代码通过 Go overlay 注入，不进入生产二进制：

```sh
python3 scripts/build-config-soak.py --managed --output artifacts/acl-stress/plain
python3 scripts/test-acl-master.py --merged --stress --concurrency 128 --rounds 6 \
  --client artifacts/acl-stress/plain/sgate-client \
  --server artifacts/acl-stress/plain/sgate-server --output artifacts/acl-stress/plain-results
```

带竞态检测时在构建命令添加 `--race`，测试指向生成的 `sgate-client-race`、`sgate-server-race`。压力测试使用默认重连退避，双端开启 10～30ms 随机间隔、128～512 字节随机白噪音，每轮在 128 路流传输中切断隧道，并交替重启客户端、服务端框架进程。隧道断线检查原业务连接续传完整性；进程重启检查旧连接结束、已有登录会话可继续使用、新连接传输正常。旧服务端会话按测试配置的 20 秒窗口保留，到期后检查释放。输出含空闲阶段内存、协程、描述符及会话/缓存采样，退出时要求会话、流、缓存和传输写协程归零，并检查子进程退出和端口可重新绑定。

单端口管理综合压力可在上述测试命令添加 `--management-stress --login-concurrency 8 --apply-rounds 6`，独立模式另加 `--mode standalone`；默认同时覆盖 WS/WSS，可用 `--tls wss` 只运行 HTTPS/WSS。每轮在 128 个业务流及管理状态采样运行期间，并发登录、交替修改白噪音大小上限、连续执行保存并应用及“仅保存＋单独应用”，再重启服务进程；托管模式使用真实 `master_ctl`。接受登录的旧 Cookie 必须跨多轮代理应用及进程替换保持有效，登录 `429` 按现有限流规则单独统计。`management-load-*.json` 保存管理请求耗时、登录/限流次数、配置应用次数、数据记录校验及计划内停机时间，不保存密码或 Cookie。主动代理/进程重启会关闭旧业务连接；该阶段核验已收到的记录前缀、完整记录及新连接，另有仅切断隧道的整包传输阶段严格要求双向无丢失、无重复。

退出诊断最多等待 5 秒让异步关闭的协程收尾，并保存 `.metrics.closed-<pid>.stack`；避免将关闭套接字瞬间尚未调度退出的读协程误判为持续泄漏。采样未能按时收敛或旧进程未退出，测试仍会失败。

独立启动回归测试同时覆盖两端的纯命令行、显式 `-alone`、配置文件及命令行覆盖，以及 WS/WSS 双向传输和退出后端口释放：

```sh
SGATE_ADMIN_CLIENT_BINARY="$PWD/bin/sgate-client" SGATE_ADMIN_SERVER_BINARY="$PWD/bin/sgate-server" \
  go test ./internal/app -run '^TestStandaloneStartModesBinaries$' -v -count=1 -timeout=2m
```

合并配置的独立管理部署验证（不同工作目录、HTTP/HTTPS、WS/WSS、配置应用和登录持久化）：

```sh
python3 scripts/test-merged-standalone.py --client bin/sgate-client --server bin/sgate-server --output artifacts/merged-standalone
```

## Web 管理界面

客户端与服务端均提供独立的管理工作台，包含实时发送/接收速率、累计流量、曲线、通道 TCP 连接数、配置编辑及账号管理。默认关闭，通过 `.cf` 的 `admin_enabled = true` 或命令行 `-admin-enabled` 启用；独立运行需提供 `-config` 或 `-f`，托管运行自动使用框架的 `-f`。先将应用配置中的地址、密钥、代理密码填写正确：

```sh
./bin/sgate-client -config sgate-client.cf -admin-enabled
./bin/sgate-server -config sgate-server.cf -admin-enabled
```

管理界面的 HTML、CSS、JavaScript 位于独立的 `web/admin/` 目录，不嵌入 Go 可执行文件。部署时复制程序、`.cf` 和 `web/`，并保留 `conf/` 作为账号与会话数据目录；也可在 `.cf` 中设置 `admin_web_dir = /opt/soft/sgate/web/admin`。合并示例设置 `admin_web_dir = web/admin`，相对 `.cf` 所在目录解析。客户端和服务端可共用同一份界面文件，也可分别指定目录。直接修改或替换界面文件后，刷新浏览器即可加载，无需编译或重启代理。目录中必须有 `index.html`、`app.css`、`app.js`，缺失时管理服务启动会明确报错；更新部署时建议原子替换文件。

管理进程需要界面目录的遍历权限、界面文件的读取权限，以及业务配置所在目录的写入权限（用于临时校验、备份和原子替换）。启动时界面文件不可访问会报告具体路径及系统错误；运行中丢失或不可读时，相应页面请求返回 `503`，管理 API 和已有登录会话仍可使用，恢复文件或权限后刷新即可。配置目录不可写时，保存失败，文件和正在运行的代理保持原状。

默认客户端管理地址为 `http://127.0.0.1:9081/`，同时支持 `/admin`（跳转至 `/admin/`）和 `/admin/`，三个入口共用账号及登录会话；服务端管理地址为代理入口的 `/admin/`，例如 `https://服务器:8080/admin/`，WSS 仍为 `wss://服务器:8080/tunnel`，只需一个 TCP 监听端口。管理界面仍须通过 `admin_enabled = true` 或 `-admin-enabled` 开启。服务端复用代理 `tls-cert`、`tls-key`，原 `admin_listen`、`admin_tls_cert`、`admin_tls_key` 不再创建监听或加载证书；保留 `admin_listen` 原值可维持旧登录 Cookie 的身份。客户端仍使用独立管理监听。管理参数以 `admin_` 为前缀，连字符改为下划线，例如 `admin_session_timeout = 12h`。业务相对路径仍按进程工作目录解析，管理存储、静态目录和证书路径相对 `.cf` 所在目录解析。合并示例中的 `conf/admin-*-users.json`、`conf/admin-*-sessions` 继续指向原存储位置。现有 SSH、VPN、Web 配置可直接作为 `-config`，无需迁移其地址或密码。

登录后点击右上角“退出登录”，服务端立即删除当前登录对应的持久化会话文件并清除浏览器 Cookie。页面停止流量刷新、清空配置草稿与密码输入，返回登录页；旧 Cookie 不能恢复该会话。其他浏览器的登录会话继续有效。

首次打开界面会主动进入“创建首个管理员”，没有预设账号和密码。程序生成一次性初始化令牌，日志只提示 `setup-token-file` 路径；从该文件读取令牌，在页面输入用户名、至少 12 个字符的密码及令牌即可创建账号。创建成功后令牌文件删除，后续直接登录。管理密码与代理的 `-password` 分开保存。账号文件存用户名、随机盐及 PBKDF2-HMAC-SHA256 密码哈希（600,000 次迭代，参照 [OWASP 密码存储建议](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html)），不保存明文密码。

管理配置示例见 [客户端](sgate-client.cf)、[服务端](sgate-server.cf)。下表是兼容的独立管理文件键名；合并配置使用 `admin_` 前缀并将 `-` 换为 `_`：

| 参数 | 用途 |
| --- | --- |
| `listen` | 客户端独立管理监听；服务端仅保留旧 Cookie 身份，不绑定 |
| `web-dir` | 外部界面目录，默认 `../web/admin`，相对管理配置文件解析 |
| `users-file` | 独立管理员账号 JSON 文件，首次建号自动生成 |
| `session-dir` | 持久登录会话目录，权限必须为 `0700`，不存在时自动创建 |
| `session-timeout` | 登录固定有效期，默认 `12h`，最短 `1s`，最长 `8760h` |
| `max-sessions` | 最大持久登录会话数，默认 `128` |
| `setup-token-file` | 首次建号令牌文件 |
| `tls-cert` / `tls-key` | 客户端独立管理 HTTPS 证书/私钥；服务端使用代理 TLS 配置 |

管理入口绑定非回环地址必须启用 HTTPS：客户端配置管理证书，服务端配置代理证书。账号、会话、令牌与配置备份文件按 `0600` 创建；管理员账号和登录会话继续与代理配置分开存储。持久 Cookie 使用 HttpOnly、SameSite=Strict，HTTPS 下同时使用 Secure。会话有效期固定，不随访问延长；进程重启或浏览器重开后，在原有效期内仍可登录。到期立即拒绝访问，后台最多每分钟清理过期文件。退出会删除对应会话；修改管理员账号/密码需要当前密码，并使全部旧会话失效。管理超时及存储路径修改后，需要重启整个程序；客户端管理监听和证书也需整进程重启。服务端代理证书可通过“保存并重启代理”更新，新 TLS 连接使用新证书；共享监听地址或 HTTP/HTTPS 模式变更需仅保存后重启整个进程，应用操作会拒绝并回滚此类变更。合并 `.cf` 中的框架参数及 `admin_` 参数变更可“仅保存”，但“保存并重启代理”会拒绝并回滚这些变更，避免误报已经生效；随后使用 `master_ctl` 或手工重启整个程序。

配置页以表单管理当前 `-config` 指定文件中的配置项，每项单独一行，按服务框架、代理与通道、Web 管理分组，显示文件中的中文说明及命令行优先标记。每行右侧的保存图标默认灰色不可点，值发生修改后启用，改回原值或保存成功后重新禁用；点击仅保存该项，不重启代理，也不覆盖其他行的未保存编辑；整页保存和应用按钮继续保留。保存时只替换修改的值，保留原有注释和未编辑的内容。代理密码及可能含密码或密钥的 `master_args`、`master_env` 使用密码输入框隐藏；不编辑则保留原值，输入新值则替换。页面不提供原始文件编辑器。保存前做语法、参数及密钥/证书校验，再备份至 `.bak` 并原子替换；旧版本编辑会被拒绝，避免覆盖其他操作。可选择“保存配置”或“保存并应用”；标注为即时生效的字段保存即更新，其他参数通过应用/重启生效。命令行显式参数在重启后仍优先，页面显示优先项名称及实际监听地址。

“保存并重启代理”只替换业务代理，管理 HTTP 服务、登录会话及本进程流量累计继续保留，应用请求可正常返回。重启会中断本端现有业务连接，远端按原会话保留机制清理；不能恢复这些已断开的业务连接。若新监听端口被占用等导致启动失败，会尝试恢复上次运行的代理配置，并在界面显示结果；“保存并重启”失败还会还原本次保存前的文件。

在 acl_master 托管模式下，配置页另有“通过 acl_master 重启服务”按钮，对启动时 `-f` 指定的服务配置执行 `/opt/soft/acl-master/bin/master_ctl -f <服务配置路径> -a restart`。例如服务端以 `-f /opt/soft/sgate/conf/sgate-server.cf` 启动时，即执行该文件的 restart。按钮使用已保存的文件，不会自动保存编辑框；可先“仅保存”框架或管理配置，再重启整个服务。整服务重启会替换进程、中断业务和管理连接，页面等待新 PID 恢复后才报告完成；原持久登录会话仍按原有效期使用。若改了管理地址、协议或证书，应手工打开新地址。独立启动时隐藏此按钮，仍可使用进程内的代理重启。

控制程序路径可在同一 `.cf` 中以 `admin_master_ctl = /opt/soft/acl-master/bin/master_ctl` 指定；使用自定义 acl_master 管理套接字时增加 `admin_master_socket = /path/to/master.sock`，未设置则使用 master_ctl 默认地址。旧独立管理配置中的对应键为 `master-ctl`、`master-socket`。相对路径按管理配置文件所在目录解析，修改这些设置需重启进程。控制命令直接执行固定参数，不经过 shell，不接受 HTTP 请求指定命令或服务文件；运行账号须拥有控制套接字权限。命令启动失败会直接返回错误，异步执行失败会记录在服务日志及运行状态中。重启请求必须携带页面读取的当前 PID，旧页面的延迟请求不能再次重启新进程；同一进程只允许一个重启任务。命令附带 `-t 30`，独立执行期限为 45 秒，不随旧代理上下文取消；退出码为 0 但旧进程仍未退出时，最多再等待 30 秒即报告未完成并允许重试。页面最多等待 90 秒，只有新 PID 已运行才提示成功。

带宽按通道 TCP 字节流统计，包含 HTTP、WebSocket、TLS、随机白噪音和隧道重发，排除 TCP/IP 包头、内核 TCP 重传包和业务侧端口；客户端独立管理流量不计入，服务端共享监听的管理 HTTP/TLS 流量计入该端口总流量；不是整机网卡总流量。曲线显示最近 60 次采样，数字每秒更新。代理重启不清空累计值，整个进程重启则重新计数。

## 构建与测试

代码按职责组织：`internal/app/definition.go` 构建有效配置，`instance.go` 管理单次运行资源，`lifecycle.go` 负责客户端启动与退出，`server.go` 管理服务端业务资源，`run.go` 处理状态与应用回滚。隧道的可靠会话与业务流在 `session.go`，单次 WebSocket 的读写、心跳和重连确认在 `transport.go`。阅读并发代码时先查看类型上的锁归属说明，再查看关闭和等待顺序。

Go 使用 `gofmt`，复杂初始化按字段换行，较长函数按生命周期阶段或独立操作拆分；场景测试保留阶段顺序与资源断言。Python 使用 Black 25.1.0，前端使用 Prettier 3.6.2，宽度设置保存在 `pyproject.toml` 和 `.prettierrc.json`。只格式化项目维护的目录，不批量改动上游 `vendor/`：

```sh
gofmt -w cmd internal third_party/gin-service
python3 -m black scripts
prettier --write web/admin
make vendor-sync
```

通过 Web 调用 master_ctl 的故障回归可在具备本机 acl_master 的环境运行 `scripts/test-acl-master.py`，在诊断二进制参数后加入 `--mode managed --merged --stress --management-stress --web-service-restart --restart-failure-checks --tls both --concurrency 128 --rounds 3 --output artifacts/web-restart-failures`。测试仅操作隔离实例，分别检查两端控制命令缺失、没有执行权限、私有管理套接字不可用、命令阻塞至真实 45 秒期限，以及每次 32 路并发点击和跨进程延迟请求；故障阶段保持 128 条业务流和管理探测，之后叠加传输断线、配置应用与连续重启。页面异步流程可使用 `node --test scripts/test-admin-restart.cjs` 验证。

需要 Go 1.23 或以上版本和 Make。普通编译无需 Python、C 编译器或 acl_master；竞态测试需要 C 编译器。

```sh
make build
```

首次构建会按 `go.mod` / `go.sum` 下载缺少的模块；也可以先执行 `make deps`。请按本机网络环境设置 `GOPROXY`，例如 `GOPROXY=https://proxy.golang.org,direct make build`。默认显式使用模块模式，避免已有但过期的 `vendor/` 覆盖本地框架扩展。

`third_party/gin-service/` 和 `third_party/go-service/` 是包含生命周期扩展的必要源码，必须与项目一起提交、拉取；不能用上游同名模块或参考示例替代。`vendor/` 不属于普通构建的前提条件。

```sh
make client                       # 仅编译客户端
make server                       # 仅编译服务端
make -j2 build                    # 并行编译两端
make build BINDIR=dist/bin         # 自定义输出目录
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 make build BINDIR=dist/linux-amd64
make framework-test               # 同时测试两份本地框架扩展
make test
make vet
make help
```

可通过 `GO` 指定 Go 程序，通过 `BUILD_FLAGS`、`LDFLAGS` 指定编译参数，通过 `TEST_FLAGS` 缩小测试范围。可从其它目录执行 `make -f /绝对路径/sgate/Makefile build`；相对 `BINDIR` 始终以项目目录为基准。`make clean` 只删除这两个输出程序，保留配置、密钥、运行数据及其它工具。

离线部署前，在可联网机器执行 `make vendor-sync`（需要 Python 3），将生成的完整 `vendor/` 随源码复制到目标机器，使用 `GOPROXY=off make build MOD=vendor`；修改本地框架后需要重新同步。或手动构建：

```sh
mkdir -p bin
go build -mod=mod -o bin/sgate-server ./cmd/sgate-server
go build -mod=mod -o bin/sgate-client ./cmd/sgate-client
go test -mod=mod -race ./...
```

安装到默认目录 `/opt/soft/sgate`：

```sh
make build
sudo make install-files   # 使用已编译的程序安装，不在 root 下重新编译
# 也可直接 sudo make install，完成编译和安装。
```

安装内容为 `sbin/sgate-client`、`sbin/sgate-server`，`bin/` 下四份通用启停脚本，`conf/` 下两份正式 `.cf` 配置和 `sgate.key`，以及 `web/admin/` 独立管理页面；同时创建 `logs/`、`var/`，保留程序及框架许可证。安装不启动服务，也不自动注册到 acl_master。SSH、VPN、Web 专用配置及原 SSH 脚本安装到 `samples/`，不会放入正式配置目录。

初次安装会将配置内的程序、日志、管理页面、账号和会话路径调整到安装目录，地址、密码、证书及业务参数保持原值。再次安装保留已有 `.cf`、密钥和登录状态，新版模板写入 `.cf.dist`，仅更新程序、页面及脚本。新装配置和密钥权限为 `0600`；使用其他运行账号时，应为该账号设置配置、日志和运行目录的所有权。开发机 `conf/` 中的管理员账号、会话和初始化令牌不会被打包。

若源码目录存在 `sgate.key`，仅在目标密钥缺失时安装它；否则生成 32 字节随机密钥。部署两端时需使用同一份密钥。示例中的外部证书和 acl_master 需另行配置。

```sh
make install PREFIX="$HOME/sgate"          # 自定义实际运行目录
make install DESTDIR=/tmp/sgate-package    # 暂存到 /tmp/sgate-package/opt/soft/sgate
```

`DESTDIR` 用于打包，写入配置的仍是 `PREFIX` 对应的运行路径。安装后的独立启动命令为 `/opt/soft/sgate/sbin/sgate-server -alone -f /opt/soft/sgate/conf/sgate-server.cf`，客户端同理；请先在配置中填写部署所需的密码和地址。通用启停脚本为 `sgate-client-start.sh`、`sgate-client-stop.sh`、`sgate-server-start.sh`、`sgate-server-stop.sh`，调用外部 `master_ctl` 并读取安装目录 `conf/` 内对应的正式配置，可用 `MASTER_CTL` 环境变量指定控制程序。原 SSH 脚本只作为 `samples/` 内的示例保留。升级安装不会自动删除旧的专用配置或脚本，迁移并确认不再使用后可自行归档。

分离界面的部署回归使用实际构建的两端程序，测试不同工作目录、HTTP/HTTPS、界面路径、文件权限恢复和配置应用。先运行 `make build`，再执行：

```sh
SGATE_ADMIN_CLIENT_BINARY="$PWD/bin/sgate-client" \
SGATE_ADMIN_SERVER_BINARY="$PWD/bin/sgate-server" \
go test ./internal/app -run '^TestManagementDeploymentBinaries$' -v -count=1 -timeout=4m
```

测试使用临时目录和回环端口。也可将这两个变量指向 `go build -race` 生成的程序进行对照；以 root 身份运行时会跳过无法真实模拟的 Unix 权限拒绝检查。

## 快速使用

先生成一个随机密钥，将同一份密钥文件安全地放到两端：

```sh
umask 077
openssl rand -hex 32 > sgate.key
```

假设应用服务器在服务端机器的 `127.0.0.1:5432` 监听。

两端必须通过 `-password` 配置相同的连接验证密码，下例中的 `your-password` 请替换为自己的密码。密码与 `sgate.key` 是两项独立配置。

服务端：

```sh
./bin/sgate-server \
  -listen :8080 \
  -target 127.0.0.1:5432 \
  -key-file sgate.key -password 'your-password'
```

客户端：

```sh
./bin/sgate-client \
  -listen 127.0.0.1:15432 \
  -server ws://SERVER_IP:8080/tunnel \
  -key-file sgate.key -password 'your-password'
```

应用客户端连接 `127.0.0.1:15432` 即可。服务端的目标地址固定在服务端配置中，客户端不指定任意转发目标。多个客户端进程可以共享同一个服务端，各自建立独立会话。

也可以在两端用 `SGATE_KEY` 环境变量传入 64 位十六进制密钥；指定 `-key-file` 时优先使用文件内容。

### 合并配置的部署回归

下面的矩阵先用旧的分离配置建立管理员账号和登录会话，再移除旧配置并切换到单份 `.cf`，检查原 Cookie 和原账号密码仍可使用：

```sh
python3 scripts/build-config-soak.py --managed --output artifacts/merged-deployment/plain
python3 scripts/test-merged-deployment.py \
  --client artifacts/merged-deployment/plain/sgate-client \
  --server artifacts/merged-deployment/plain/sgate-server \
  --output artifacts/merged-deployment/plain-results
```

覆盖独立/隔离 acl_master、WS/WSS、相对/绝对路径八种组合。工作目录与配置目录位于不同深度，重启后切换工作目录；应用密钥和证书按工作目录解析，管理存储、证书和外部页面按 `.cf` 目录解析。每组含 32 路字节完整性检查、命令行覆盖、配置保存/代理应用、进程重启及资源采样。测试要求旧账号和会话文件保持原样、退出会话/流/缓存清空、传输写协程退出、进程消失、端口可重新绑定；`lsof` 留档检查工作进程未持有框架管理套接字。诊断采样仅存在于 overlay 构建中。

构建加 `--race` 后使用 `sgate-client-race`、`sgate-server-race` 可重复竞态对照。`--mode managed --tls ws --paths relative --restarts 3` 可单独验证连续托管重启；其他值见脚本 `--help`。Unix 托管启动只接管 FD 5 状态通道及 FD 6 起的监听套接字，清理框架额外继承且未设置 CLOEXEC 的套接字，避免工作进程持有 `master_ctl` 管理连接；不关闭标准流、普通文件、管道及 Go 自有 CLOEXEC 套接字。

### 应用配置文件

两端均支持 `-config`，所有现有运行参数都可以写入配置文件，键名与命令行参数相同，省略前导 `-`。优先级为：**显式命令行参数 > 配置文件 > 内置默认值**。托管模式自动读取 `-f` 指定的服务文件；未指定 `-config` 和 `-f` 时仅使用命令行参数，不搜索文件。

完整示例见 [客户端配置](sgate-client.cf) 和 [服务端配置](sgate-server.cf)。先修改密码、地址和密钥路径，再启动：

```sh
./bin/sgate-server -config sgate-server.cf
./bin/sgate-client -config sgate-client.cf -listen 127.0.0.1:2901
```

合并配置为一个 `service 名称 { ... }` 块，UTF-8 文本，每行一个 `key = value`（不超过 4095 字节），支持空行、以 `#` 或 `;` 开头的整行注释。例如：

```ini
service example-client {
server = wss://gateway.example.com:8443/tunnel
key-file = /opt/soft/sgate/conf/sgate.key
password = 'replace-with-your-password'
heartbeat = 20s
heartbeat-timeout = 90s
noise-interval = 0
noise-interval-min = 1s
noise-interval-max = 3s
noise-min-bytes = 1024
noise-max-bytes = 10240
admin_enabled = false
}
```

时间沿用命令行格式（如 `250ms`、`3s`、`2m`），大小为整数、单位为字节。字符串可以不加引号；需要保留首尾空白时使用引号，单引号内容原样保留，双引号支持 `\"`、`\\` 等 Go 字符串转义。`.cf` 通过服务模板 `Config.InitConfig/GetString` 读取；未加引号的值支持以空格加 `#` 开始的行尾注释，密码中的字面 `#` 请放在引号内。旧独立 `.conf` 不支持行尾注释。未知键、重复键、文件语法错误、最终生效的参数值无效或文件读取失败会中止启动。命令行已覆盖的文件值不再进行类型转换。相对文件路径相对于进程工作目录解析。

命令行覆盖按每个参数独立处理，显式 `0` 或空字符串也会生效。例如文件已开启随机间隔白噪音时，关闭发送使用 `-noise-interval-min 0 -noise-interval-max 0`；切换为固定间隔还需加上 `-noise-interval 1s`。固定间隔和随机范围仍须满足原有互斥校验。未提供有效 `key-file` 时继续使用 `SGATE_KEY` 环境变量。

正式配置为根目录的 `sgate-client.cf`、`sgate-server.cf`；原 SSH、VPN、Web 专用配置位于 `samples/`。旧 `conf/*.conf` 已合并并移除，`conf/` 仍可放密钥、管理员账号、会话及令牌等数据文件。程序保留旧独立 `.conf` 的兼容读取，以及显式 `-admin-config` 指定外部管理配置的能力。

部署时把所需 `.cf` 安装到 acl_master 的服务配置目录，权限设为 `0600`，并保留程序对该目录的写权限以使用 Web 保存功能。升级前先停止旧服务，再安装新版程序和合并配置，最后启动；管理参数相对路径以安装后的 `.cf` 目录为基准，部署到不同目录时应改为原存储位置的绝对路径，以继续使用已有账号和会话。原业务地址、密码和证书路径保持不变；Web 客户端原密码为空，部署前仍须填写或通过 `-password` 提供。

`master_args` 可留空，也可直接写覆盖项，例如 `master_args = -noise-interval-min 1s -noise-interval-max 3s`。托管监听仍由 `master_service` 决定；业务 `-listen` 只在独立运行时改变实际绑定端口。不要再将 `master_args` 指向已删除的 `conf/*.conf`。

### 配置白噪音与重连回归

可用实际二进制验证配置文件开启白噪音、命令行修改区间/大小、单端及双端关闭，覆盖 WS/WSS 和每场景三次断线重连：

```sh
mkdir -p artifacts/config-noise-reconnect
go build -o artifacts/config-noise-reconnect/sgate-client ./cmd/sgate-client
go build -o artifacts/config-noise-reconnect/sgate-server ./cmd/sgate-server
SGATE_CONFIG_CLIENT_BINARY="$PWD/artifacts/config-noise-reconnect/sgate-client" \
SGATE_CONFIG_SERVER_BINARY="$PWD/artifacts/config-noise-reconnect/sgate-server" \
  go test -race ./internal/tunnel -run '^TestConfigBinaryNoiseReconnect$' -count=1 -v -timeout=4m
```

两条 `go build` 均加 `-race` 可验证竞态构建，使用独立输出文件并串行对照。回归包含加速间隔和真实 `1s`～`3s`、`1024`～`10240` 字节配置；本地观测中继使用临时密钥解密检查白噪音，不修改加密数据帧，WSS 两段均校验证书。中继分别处理其两段的控制帧，本回归不用于证明跨中继的端到端心跳时序。每次重连均要求启用的方向重新发送白噪音，关闭方向保持零发送；四条原应用流使用独立内容、序号、终止标记和 EOF 校验。缓存与定时器内部释放另由 `TestNoiseDirectionTimersReleaseOnExpiry` 及 `TestNoisePendingTimerReleasedOnExpiryAndShutdown` 检查，包含尚需一小时才会触发的定时器。

配置白噪音的生命周期回归使用同一组二进制环境变量，将测试名改为 `^TestConfigBinaryNoiseLifecycle$`，并添加 `-parallel=6 -timeout=6m`。每档共 18 项：WS/WSS × 文件开启/CLI 修改/CLI 双端关闭 × 长断网/服务端进程重启/会话过期。长断网保持真实 `2m` 客户端和服务端保留期，持续阻断 125 秒，要求旧应用与后端连接在断网期间按原期限自然关闭；快速过期单独使用 `750ms` 服务端期限和 `30s` 客户端期限，不替代两分钟用例。

生命周期用例在断网后从旧流两个方向各提供 2 MiB 未确认数据，恢复后使用不同内容的新流检查序号、双向数据、终止标记、EOF 和新会话的最终 ACK。到期及服务端重启会按设计中断旧流，未确认的旧数据不会混入新流。中继保留原 HTTP 错误状态及 `X-Sgate-*` 头，尤其不能将带鉴权证明的 410 响应改成 503。`TestConfigNoiseLifecycleResources` 以短窗口重复检查实际内部流表、重放队列及写协程释放，额外包含一至两小时的待触发白噪音定时器。

`TestConfigBinaryNoiseWeakNetwork` 使用相同二进制环境变量和 `-parallel=6 -timeout=4m`，覆盖 WS/WSS × 文件开启（1～3 秒、1～10 KiB）/CLI 修改（100～300 毫秒、128～512 字节）/CLI 双端关闭。透明 TCP 中继原样转发 TLS、WebSocket 数据及控制帧，不代答 Ping/Pong。每例先在单向延迟 150±100 毫秒、15% 分段延迟重传（额外 200 毫秒）下运行 22 秒，超过 18 秒心跳超时；再分别阻断上行、下行 1 秒，要求均不误重连，最后逐向持续阻断直到自动重连并恢复。两条原应用连接持续发送独立的双向序号记录，校验内容、结束标记和 EOF，要求恢复后不新增后端连接。丢包模型保留 TCP 字节顺序，通过重传延迟模拟恢复，不代表真实内核丢包测试。

`TestConfigNoiseWeakNetworkResources` 对匹配的有效参数状态检查私有流表、重放缓存、会话过期及传输写协程退出；额外在单向断网过期时检查 1～2 小时待触发定时器取消。该测试与 CLI 参数优先级测试、实际二进制参数及白噪音大小测试共同使用；它自身不调用配置解析器。中继的 `waitTimers` 是累计创建次数，不是活动定时器数量；释放依据是所有中继协程结束、队列清空及传输写协程退出。

### 配置白噪音综合压力测试

使用 `python3 -B scripts/build-config-soak.py` 构建普通诊断程序，再加 `--race` 构建竞态诊断程序。构建工具通过 Go overlay 给原命令入口注入统计，直接执行原配置解析、命令行参数和代理逻辑，不修改生产源码；生成文件使用 `.overlay` 后缀，避免被 `go test ./...` 扫描。诊断程序仅用于测试。

```sh
SGATE_CONFIG_SOAK=1 \
SGATE_CONFIG_CLIENT_BINARY="$PWD/artifacts/config-noise-soak/sgate-client" \
SGATE_CONFIG_SERVER_BINARY="$PWD/artifacts/config-noise-soak/sgate-server" \
SGATE_CONFIG_SOAK_REPORT_DIR="$PWD/artifacts/config-noise-soak/plain" \
GOMAXPROCS=4 go test ./internal/tunnel -run '^TestConfigBinaryNoiseSoak$' \
  -parallel=6 -count=1 -timeout=8m -v
```

竞态档单独运行，将两端二进制都换成 `-race` 后缀，使用独立报告目录，并将测试命令改为 `go test -race`。六组并行覆盖 WS/WSS × 文件开启/CLI 修改/CLI 双端关闭，每组 128 条应用流，分四轮各 75 秒运行，每例累计 300 秒负载。透明 TCP 中继叠加单向 150±100 毫秒延迟、15% 分段延迟重传（额外 200 毫秒），每六秒断开长连接；保留生产默认重连退避及两分钟会话保留期。两端独立发送序号、随机内容、结束标记和 EOF，轮次之间使用不同种子。文件参数为 1～3 秒、1～10 KiB；CLI 修改为 100～300 毫秒、128～512 字节，关闭时显式清零随机间隔两端边界。

每两秒采样堆、RSS、协程、流表、重放缓存、nonce 缓存和传输写协程；每轮结束排空流及缓存，再强制 GC 比较同等状态。断言包括：每端只保留一个传输写协程，GC 后堆相对第一轮增长不超过 8 MiB，协程不超过基线加 12，描述符不超过基线加 4，服务端 nonce 不超过 64。报告保留完整时间序列，仍需分析趋势，不能只以阈值未超限判断没有泄漏。macOS 描述符统计在静止采样时回退到 `lsof`。最终退出要求会话、流、缓存、nonce 和传输写协程全部归零，中继连接和排队字节归零。定时器释放以写协程结束为依据，不枚举运行时定时器。统计本身每次分配固定 4 MiB 协程栈采样缓冲并调用 `ps`，因此按相同负载及 GC 状态比较。此为五分钟负载档，丢包通过 TCP 有序重传延迟模拟；不代表通宵或内核真实丢包测试。`SGATE_CONFIG_SOAK_SMOKE=1` 仅用于四条流的短预检，结果应分开记录。

### HTTP 请求头密码验证

客户端在 HTTP/HTTPS WebSocket 握手请求中发送 `X-Sgate-Password: v1.<Base64URL密文>`。密码使用 `sgate.key` 所指定的 256 位密钥派生独立 AES-256-GCM 密钥后加密，每次握手和重连都生成新的随机 nonce。会话 ID、请求随机数、时间戳、恢复标志和请求 URI 参与认证，密文不能移到另一次握手中使用；请求签名及服务端响应证明也绑定该密文。

服务端在升级为 WebSocket 和分配代理会话之前解密、核对密码。缺失、错误、篡改或重复的密码头均拒绝；当前服务端进程防重放窗口内的握手重放也会被拒绝。拒绝时统一返回 HTTP 404，不创建代理会话或目标 TCP 连接。两端启动时均要求非空密码，最长 1024 字节；密码不会以明文出现在请求头中。普通 HTTP 首页仍可访问。

此版本需同时更新客户端和服务端，并为两端补充 `-password`；旧版仅使用密钥的握手不再接受。

### TLS

对外部署建议使用 `wss://`，同时保护 HTTP 握手信息。使用受客户端信任的证书和与证书匹配的域名：

```sh
./bin/sgate-server \
  -listen :8443 -target 127.0.0.1:5432 -key-file sgate.key -password 'your-password' \
  -tls-cert server.crt -tls-key server.key

./bin/sgate-client \
  -listen 127.0.0.1:15432 \
  -server wss://gateway.example.com:8443/tunnel \
  -key-file sgate.key -password 'your-password'
```

私有 CA 可以在客户端加上 `-tls-ca ca.crt`。客户端会校验证书链和服务器名称。

### 客户端选择后端服务

一个服务端进程可以为不同客户端代理不同的后端端口。在服务端 `.cf` 中保留默认后端，并列出允许客户端选择的地址：

```ini
# 未传 addr 时使用的默认后端，仍为必填项。
target = 127.0.0.1:9001
# 完整 host:port 允许列表，逗号分隔；留空禁止客户端指定后端。
allowed-targets = 127.0.0.1:8088,127.0.0.1:22,backend.example.com:443
```

客户端可以单独设置后端地址，程序会编码为握手 URL 中的 `addr` 参数：

```ini
server = "wss://gateway.example.com/tunnel"
addr = 127.0.0.1:8088
send-addr = true
```

`send-addr` 默认 `true`，保持上述地址选择行为。设为 `false` 时，忽略独立 `addr` 并移除请求 URL 中所有 `addr` 参数，使用服务端默认后端，其他查询参数保留；配置中保存的地址不会被清空。命令行 `-send-addr=false` 或 `-send-addr=true` 优先于文件，可在管理界面修改并保存应用；应用会关闭受影响的旧应用连接。

也可使用命令行 `-addr backend.example.com:443`；命令行显式参数优先于配置文件。非空的独立 `addr` 优先于 `server` URL 中的同名参数，其他查询参数保留。独立 `addr` 留空时兼容原来的 `server = "wss://gateway.example.com/tunnel?addr=127.0.0.1:8088"` 写法；两处均未指定才使用服务端默认后端。切回默认后端时，应清空独立 `addr` 并移除 URL 的 `addr`。客户端会提前拒绝非法地址、空值或重复的 URL `addr`；是否允许访问仍由服务端允许列表决定。

域名示例为 `?addr=backend.example.com:443`，IPv6 使用 `[::1]:8088`；构造 URL 时可对参数值作 URL 编码。地址按完整字符串精确匹配，不支持通配符、网段或通过 DNS 解析结果匹配别名。即使显式指定默认后端，也必须把该地址列入 `allowed-targets`。参数为空、重复或格式错误返回 `400`，未获允许返回 `403`；这些检查在握手身份验证通过后、创建会话和连接后端前完成。

后端绑定到隧道会话，同一会话的所有 TCP 流和重连均保持该目标；重连试图换成另一后端返回 `409`，不会续期或改变原会话。要切换目标，应修改客户端 `addr` 或 URL，并在管理页面保存应用或重启客户端代理，建立新会话。允许列表同时支持 `-allowed-targets` 命令行覆盖和管理页面编辑；保存后应用代理配置即可生效，应用会关闭旧代理会话。

### HTTP 反向代理

也可以让 Nginx 提供 HTTPS，sgate 服务端监听 `127.0.0.1:8080`。在已有 HTTPS `server` 块中加入：

```nginx
location /tunnel {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_read_timeout 90s;
    proxy_send_timeout 90s;
    proxy_buffering off;
}
```

客户端连接 `wss://gateway.example.com/tunnel`。代理需保留鉴权请求及响应头，并将同一个会话路由到同一个 sgate 服务端进程。客户端支持标准 HTTP 代理环境变量。

## 参数

公共参数：

| 参数 | 默认值 | 含义 |
| --- | --- | --- |
| `-config` | 无 | 合并的服务 `.cf`（兼容旧 `.conf`）；省略时使用框架 `-f` |
| `-admin-enabled` | false | 启用同一 `.cf` 中的 `admin_` 管理配置；可显式设 false 关闭 |
| `-key-file` | 无 | 密钥文件；未指定时读取 `SGATE_KEY` |
| `-password` | 必填 | 连接验证密码，两端保持一致，1～1024 字节 |
| `-max-streams` | `128` | 每个会话允许的 TCP 流数 |
| `-buffer-bytes` | `4194304` | 每个方向未确认帧的缓存上限，至少 `16409` 字节 |
| `-heartbeat` | `20s` | WebSocket 心跳间隔 |
| `-heartbeat-timeout` | `0` | 接收空闲超时；`0` 表示心跳间隔的三倍，默认 `60s` |
| `-noise-interval` | `0` | 随机填充发送间隔，如 `1s`；`0` 禁止本端发送 |
| `-noise-interval-min` / `-noise-interval-max` | `0` | 随机等待时间的上下限，如 `1s` / `3s`；须同时设置，且不能与非零 `-noise-interval` 混用 |
| `-noise-min-bytes` | `1024` | 随机填充负载最小字节数 |
| `-noise-max-bytes` | `10240` | 随机填充负载最大字节数，最高 `10M`（10485760 字节） |

服务端参数：

| 参数 | 默认值 | 含义 |
| --- | --- | --- |
| `-listen` | `:8081` | HTTP/HTTPS 监听地址 |
| `-target` | 必填 | 未传 `addr` 时默认连接的 `host:port` |
| `-allowed-targets` | 空 | 客户端 `addr` 可选地址的完整允许列表，逗号分隔；空值禁止选择 |
| `-path` | `/tunnel` | WebSocket 路径 |
| `-session-ttl` | `2m` | 传输连接断开后保留会话的时间 |
| `-max-sessions` | `128` | 活跃及保留会话总数上限 |
| `-tls-cert` / `-tls-key` | 无 | HTTPS 证书及私钥，必须同时提供 |

客户端参数：

| 参数 | 默认值 | 含义 |
| --- | --- | --- |
| `-listen` | `127.0.0.1:9000` | 应用客户端连接的本地 TCP 地址 |
| `-server` | 必填 | 完整 `ws://` 或 `wss://` URL |
| `-addr` | 空 | 服务端连接的可选后端 `host:port`；非空覆盖 URL 同名参数，空值保留 URL 选择 |
| `-send-addr` | `true` | 是否携带目标地址；`false` 忽略独立 addr 并移除 URL addr，使用服务端默认后端 |
| `-reconnect-min` | `250ms` | 初始重连退避上限，实际等待为其 50%～100% |
| `-reconnect-max` | `10s` | 最大重连退避上限 |
| `-offline-timeout` | `2m` | 客户端持续离线时保留应用连接和未确认数据的时限 |
| `-tls-ca` | 无 | 额外信任的 PEM CA 证书 |

## 随机流量填充

客户端和服务端都支持周期性发送随机内容、随机大小的加密填充帧。例如在需要发送填充的一端增加：

```sh
-noise-interval 1s -noise-min-bytes 1024 -noise-max-bytes 10240
```

这表示每秒调度一次填充，负载大小在 1～10 KiB 间均匀随机选择（包含上下限）。也可使用 `500ms` 等 Go 时间格式；大小参数支持整数字节数或 K/M 后缀（1K=1024，1M=1048576），必须满足 `1 <= min <= max <= 10485760`。两端独立配置，可以单端发送，也可以双端发送；关闭发送的一端仍能接收并丢弃填充。使用 acl-master 时可直接编辑 `.cf` 的白噪音配置项，也可通过 `master_args` 中的命令行参数覆盖。

如需发送间隔也随机变化，改用以下参数：

```sh
-noise-interval-min 1s -noise-interval-max 3s \
  -noise-min-bytes 1024 -noise-max-bytes 10240
```

连接建立后，以及每次填充发送完成后，均在 1～3 秒范围内独立、均匀随机选择下一次等待时间（纳秒精度，包含上下限），包大小也每次独立随机生成。间隔上下限须满足 `0 < min <= max`，相等时等效于固定等待。范围参数须成对设置，并移除原有的非零 `-noise-interval`；不设置固定间隔或范围参数时仍默认关闭。网络拥塞可能延长实际间隔，积压的周期不会补发。

填充与应用数据一样封装为 AES-GCM 加密的 WebSocket 二进制消息，接收端解密校验后直接丢弃，不转发给应用，不占用可靠帧序号、应用连接或重传缓存。即使没有应用连接，只要 WebSocket 已建立也会发送。应用数据和心跳仍会调度；网络拥塞或写阻塞会推迟填充，错过的周期不会积压补发。传输断开后计时器停止，重连后重新选择等待时间，不重放旧填充。两端需升级到支持填充帧的版本后再开启，旧版本无法识别此帧类型。

大小指随机负载，不包含 25 字节协议头、24 字节加密开销以及 WebSocket、TLS、TCP/IP 的开销；WebSocket 消息也不对应固定数量或大小的网络数据包。每秒发送 1～10 KiB 的平均额外负载约为每方向 5.5 KiB/s，服务端按每个在线会话分别产生。填充会消耗带宽，无法保证隐藏协议或抵抗流量分析，实际业务的时序和大小仍可能被观察。

## 断线恢复的边界

“应用无感”指短暂的 WebSocket 中断期间，两端保留原有 TCP 套接字，并在恢复后继续按序传输，不要求应用重新连接。中断时应用读写可能暂时阻塞。

- 默认在服务端检测到连接断开后保留会话 2 分钟，可用 `-session-ttl` 调整。
- 客户端检测到传输断开后，默认也只保留旧连接和缓存 2 分钟，可用 `-offline-timeout` 调整。持续断网超过该时限时，客户端主动关闭旧连接并清空缓存，继续重连；无需等待网络恢复或服务端返回 410。尚未首次连接成功时，等待中的应用连接也受此限制。HTTP/TLS 握手卡住或重连退避不会延长保留时限；在线连接不会因这个时限被关闭。
- 有效恢复窗口由客户端和服务端共同限制，延长窗口时应同时调整 `-offline-timeout` 和 `-session-ttl`。静默丢包时，离线保留时限从心跳等机制检测到掉线后开始计算。
- 应用和目标服务器必须仍保持原有 TCP 连接；应用自己的请求/空闲超时仍然生效。
- 会话及未确认数据保存在内存中。任一进程重启、会话过期或目标 TCP 断开，都无法恢复原有 TCP 流。会话丢失时客户端关闭旧连接，并建立新会话供后续 TCP 连接使用。
- 会话过期的 HTTP 410 响应也需要通过服务端签名校验；普通代理返回的 410 只触发重试，不会清空现有连接。升级时请同时更新两端，旧版服务端的无签名过期响应不会被新版客户端接受。
- 确认表示数据已经写入目标 TCP 套接字或已处理对应控制帧，并不表示目标业务完成了事务。
- 默认每 20 秒发送心跳，等待接收时连续 60 秒没有消息内容读取进展、Ping 或 Pong 则检测为掉线。慢速接收会按消息内容的实际读取进展刷新期限，无需等到整个 WebSocket 消息接收完毕；本地 TCP 处理完成后重新计算下一次等待接收的窗口。持续发送数据时也会调度心跳。
- 高延迟链路可在两端设置如 `-heartbeat 10s -heartbeat-timeout 90s`。超时窗口须大于心跳间隔、目标连接超时和单次写超时，并为最大预期静默间隔、往返延迟、抖动和重传留出余量；TLS 链路还需考虑完整 TLS 记录的交付时间。超过配置窗口仍无接收进展时会正常重连；不保证无限延迟下维持连接。重连时若旧传输尚未退出，服务端会暂时拒绝重复接入，客户端继续重试。
- 目标连接超时为 10 秒，单次 TCP/WebSocket 写超时为 15 秒。慢目标可能使同一会话的其他流短暂等待。
- 标准 HTTP/WebSocket 行为允许正常的服务器和反向代理处理流量；加密及 HTTP 页面不保证流量不可识别。`ws://` 的握手头部仍可见。

## 协议与实现

每个 WebSocket 二进制消息包含一个加密帧。明文头为 `op(1) | seq(8) | ack(8) | stream(8)`，使用大端编码，数据块最多 16 KiB。操作包括 `ACK`、`OPEN`、`DATA`、`FIN`、`RESET`、`NOISE`。每个方向使用独立、跨重连连续的可靠帧序号，纯 ACK 和填充不占可靠序号；重新连接后重传未确认帧，接收方根据序号去重。`NOISE` 的 `seq`、`ack`、`stream` 必须为零，每个线帧负载为 1～16384 字节，仍参与当前传输的加密计数器校验。一次噪音总量最多 10 MiB，超过 16 KiB 时拆成多个加密 NOISE 帧，帧间处理业务、ACK、心跳和取消；噪音不进重放缓存，断线时剩余噪音直接放弃。

加密层每次传输从计数器 1 开始，计数器构造 GCM nonce，并严格检查顺序。会话 ID、双方随机数及方向参与派生加密密钥，因此不同传输不会重复使用同一密钥与 nonce。握手签名绑定协议版本、会话 ID、随机数、时间戳、恢复标志及请求 URI。

核心代码在 `internal/tunnel`；入口在 `cmd/sgate-client` 和 `cmd/sgate-server`。测试覆盖数据篡改和重放、错误密钥、多路复用、半关闭、大数据恢复、上下行丢失确认后的去重、双向独立数据连续断线 8 次、缓存背压、流重置解除等待、取消后的连接释放、会话过期及目标不可用。确认队列清空后释放底层数组，流重置时唤醒等待缓存的生产者。

恢复场景回归还包括：断网期间双向缓存满、客户端主动离线过期、静默丢包后心跳超时、HTTP 握手卡住、服务端在同一端口重启、连续五次重启，以及恢复后新连接不接收旧会话的残留数据。可以按真实默认两分钟窗口执行长断网用例：

```sh
SGATE_RECOVERY_LONG=1 go test -race \
  -run '^TestLongRecoveryDefaultOfflineWindow$' -v -timeout 3m ./internal/tunnel
```

随机填充开启后的恢复回归：

```sh
go test -race -count=3 \
  -run '^(TestNoiseRecoveryScenarios|TestNoisePendingTimerReleasedOnExpiryAndShutdown)$' \
  -v -timeout 1m ./internal/tunnel

# HTTPS 下真实默认两分钟断网，填充间隔 1～3 秒、负载 1～10 KiB
SGATE_RECOVERY_LONG=1 go test -race \
  -run '^TestNoiseLongRecoveryDefaultOfflineWindow$' -v -timeout 3m ./internal/tunnel
```

覆盖 HTTP/HTTPS 的双向随机填充、断网期间双向缓存满、服务实例同端口重启、连续五次重启、服务端先过期及客户端离线过期。恢复后校验新连接的完整数据、EOF 和后端字节总数，确认旧缓存没有混入新连接，并确认新的填充仍能发送。断开时等待旧传输及其写协程退出，验证停止填充定时器的清理路径已完成；长断网期间旧会话的填充计数保持不变。额外将首次填充等待设为 1～2 小时，确认过期与关闭无需等待定时器到期。每个场景清理后检查协程和文件描述符回落。

同一流的读取与写入失败可能并发上报，RESET 按流去重，避免重复重置挤满控制队列并终止整个会话。

## 资源压力测试

```sh
make stress

# 持续三分钟，跨过两分钟的握手防重放缓存保留窗口
SGATE_STRESS_DURATION=3m GOMAXPROCS=4 make stress

# 双向开启随机填充，验证并发与断线时的资源释放
SGATE_STRESS_NOISE_INTERVAL=5ms make stress

# 随机等待、随机负载、上下行独立随机数据；持续三分钟
SGATE_STRESS_DUPLEX=1 SGATE_STRESS_DURATION=3m \
  SGATE_STRESS_NOISE_INTERVAL_MIN=1ms SGATE_STRESS_NOISE_INTERVAL_MAX=5ms \
  SGATE_STRESS_NOISE_MIN_BYTES=1024 SGATE_STRESS_NOISE_MAX_BYTES=10240 \
  GOMAXPROCS=4 make stress

# 为压力场景开启竞态检测
SGATE_STRESS=1 GOMAXPROCS=4 go test -race \
  -run '^TestStressReconnectResources$' -v -timeout 10m ./internal/tunnel
```

默认使用 4 个客户端，每端 64 路 TCP 流，共 256 路并发连接，重复 12 轮；每次交换 128 KiB 数据，每 30 ms 注入一次传输断线，模拟 10 ms 网络不可用。每轮校验完整回传数据和 EOF，然后确认连接及未确认队列已释放。设置持续时间后，同一批连接反复交换数据直至该轮时间结束，再半关闭和周转连接。

可通过 `SGATE_STRESS_CLIENTS`、`SGATE_STRESS_STREAMS`、`SGATE_STRESS_ROUNDS`、`SGATE_STRESS_BYTES` 和 `SGATE_STRESS_DURATION` 调整负载。`make stress` 默认写入 `artifacts/stress.json`，可用 `SGATE_STRESS_REPORT` 指定报告路径；直接调用 `go test` 时也需显式设置该变量才保存 JSON。

`SGATE_STRESS_DUPLEX=1` 让每条连接两端独立发送不同的伪随机字节流，以记录序号、逐字节比较、结束标记及 EOF 检测丢失、重复、损坏和尾部截断，并分别核对上下行发送与接收总数。报告的 `verified_upload_bytes` / `verified_download_bytes` 记录该模式的数据量；默认回显模式仍使用 `verified_bytes_per_direction`。测试同时检查客户端和服务端的流与重传缓存，并记录双方实际发送、接收的填充帧数；填充允许在掉线时丢弃，因此不要求填充计数相等。

独立双向模式默认给压测器控制的 socket 设置 16 KiB 收发缓冲，并在每条发送流的记录之间等待 2 秒；可通过 `SGATE_STRESS_SOCKET_BUFFER`（`0` 恢复系统默认）和 `SGATE_STRESS_RECORD_INTERVAL` 调整。所有代理与应用角色在同一台机器运行，会放大本机内核缓冲压力；不限制突发速率时，256 路、128 KiB 双向突发可能先耗尽 macOS 的网络缓冲，产生与代理实现无关的系统丢包和超时。测试保留并发数、单条记录大小和断线频率，用有界缓冲与发送速率区分代理行为和本机容量限制，不更改系统参数。macOS 报告记录全系统网络内存申请失败累计计数 `network_memory_denied`（不可用时为 `-1`），应比较采样差值；该值也可能受其他进程影响。长时间不推进时会在报告旁保存 `.stacks` 协程诊断文件。

随机等待范围通过 `SGATE_STRESS_NOISE_INTERVAL_MIN` / `SGATE_STRESS_NOISE_INTERVAL_MAX` 设置，负载范围通过 `SGATE_STRESS_NOISE_MIN_BYTES` / `SGATE_STRESS_NOISE_MAX_BYTES` 设置。`SGATE_STRESS_CHURN_INTERVAL` 和 `SGATE_STRESS_OFFLINE_DURATION` 可覆盖默认的 `30ms` / `10ms`。例如测试真实的 1～3 秒随机等待时，将断线间隔设为 `4s`，使传输有机会真正发送填充；每 `30ms` 重置传输会不断重启首次等待，无法覆盖这种配置的填充发送行为。

报告记录活跃期和空闲期的堆内存、栈、协程、实际文件描述符、TCP 连接数、会话数、重传缓存和鉴权随机数缓存数量；空闲采样执行 GC，并记录 RSS。macOS 的文件描述符计数使用 `lsof`，RSS 使用 `ps`。Go 运行时保留的页和竞态检测器的影子内存会计入 RSS；通过多轮空闲堆和 RSS 趋势及连接、协程回落情况判断是否持续增长。防重放缓存按两分钟窗口保留，关闭服务端时清空。测试会断言每轮资源回落，并检查最终关闭后的连接及协程释放。

## 弱网回归测试

```sh
make weaknet
```

使用本机有界 TCP 中继注入单向 150ms 延迟、±100ms 抖动和 15% 分段重传等待（200ms），并验证空闲心跳、并发随机数据、慢速分段接收、双方向短时阻塞、单向长断网后的重连和会话过期释放。中继以每段最多 512 字节、有界队列调度，保持 TCP 字节顺序；丢包用延迟重传来模拟，不会把删除 TCP 流字节误当作 IP 丢包，也不修改系统网卡或内核网络规则。

每轮校验随机数据逐字节一致、后端收取字节总数和半关闭后的 EOF；在保留窗口内继续使用同一应用 TCP 连接，超过窗口则释放旧连接和重传缓存，恢复后新连接正常通信。上行中断时，下行已送达的字节可能仍在应用的 TCP 接收缓冲中，测试先读取这一合法前缀，再确认 EOF 或连接重置。清理阶段检查中继连接、排队字节、应用连接、会话缓存和协程均正常释放。

`make weaknet` 同时执行开启随机白噪音的 HTTP/HTTPS 场景，填充大小为 1～10 KiB。密集测试使用 100～200ms 随机间隔；慢速分段测试使用 300～600ms，避免填充本身超过模拟链路容量；独立双向测试以每方向约 40 KiB/s 的应用输入配合 200～400ms 填充间隔，另有真实 1～3 秒间隔测试，以及 1～3ms 密集填充下的繁忙写队列心跳测试。健康连接断言没有重连，双向独立数据流使用不同随机种子、连续序号、结束标记和 EOF 检查丢失、重复及尾部截断；短时单向阻塞保持连接，较长阻塞触发重连并复用原应用连接，超过保留窗口则释放旧状态。测试确认弱网期间两端确实收到填充，并在关闭后等待传输协程退出，以验证其白噪音定时器和心跳定时器已经停止，同时检查文件描述符和协程回落。TLS 慢速分段场景使用 1 秒心跳超时，以覆盖完整 TLS 记录的接收时间。

仅运行白噪音弱网场景：

```sh
go test -race -count=3 -run '^TestNoiseWeakNetwork' -v -timeout 6m ./internal/tunnel
```

### 综合长时间弱网压力测试

下面将 4 个客户端、每客户端 64 条应用 TCP 连接与双向独立数据流、每 4 秒断线、随机白噪音及弱网叠加，运行六轮、至少十分钟的应用负载：

```sh
SGATE_STRESS=1 SGATE_STRESS_DUPLEX=1 SGATE_STRESS_WEAKNET=1 \
SGATE_STRESS_CLIENTS=4 SGATE_STRESS_STREAMS=64 SGATE_STRESS_ROUNDS=6 \
SGATE_STRESS_DURATION=10m SGATE_STRESS_BYTES=1024 \
SGATE_STRESS_CHURN_INTERVAL=4s SGATE_STRESS_OFFLINE_DURATION=100ms \
SGATE_STRESS_NOISE_INTERVAL_MIN=200ms SGATE_STRESS_NOISE_INTERVAL_MAX=600ms \
SGATE_STRESS_REPORT="$PWD/artifacts/noise-combined-long.json" \
go test -run '^TestStressReconnectResources$' -v -count=1 -timeout 15m ./internal/tunnel
```

弱网默认值为单向 150ms 延迟、±100ms 抖动、15% 模拟分段丢包后的 200ms 重传等待。可用 `SGATE_STRESS_WEAK_DELAY`、`SGATE_STRESS_WEAK_JITTER`、`SGATE_STRESS_WEAK_LOSS_PERCENT`、`SGATE_STRESS_WEAK_RETRANSMIT` 覆盖。中继保持 TCP 字节有序，未注入真实 IP 丢包。双向模式默认每条连接每两秒发送一个记录，此处使用 1 KiB 记录，控制叠加填充后的输入速率。

JSON 报告增加中继连接、排队字节、累计接入和模拟重传次数，以及空闲/关闭时传输写协程数量。每轮关闭应用连接后检查两端流和重传缓存归零、旧中继连接释放、协程与描述符回落；最终等待两端传输退出，以确认其白噪音定时器与心跳定时器停止，检查所有中继连接及排队数据归零。堆采样包含测试自身保存的报告数组；该数组随采样点增加，分析时结合 RSS、代理状态和关闭后的结果判断泄漏。可增加 `-race` 做竞态验证；竞态检测器的 RSS 应单独比较。

开启 `SGATE_STRESS_TLS=1` 后使用 HTTPS/WSS，并信任测试服务端证书、验证地址和证书链；不会跳过证书校验。`SGATE_STRESS_LIFECYCLE=1` 要求双向独立模式和至少两轮，在轮次之间交替注入服务端实例重启、会话过期，最后一轮始终验证恢复后的正常数据流。例如：

```sh
SGATE_STRESS=1 SGATE_STRESS_DUPLEX=1 SGATE_STRESS_TLS=1 \
SGATE_STRESS_WEAKNET=1 SGATE_STRESS_LIFECYCLE=1 \
SGATE_STRESS_CLIENTS=4 SGATE_STRESS_STREAMS=64 SGATE_STRESS_ROUNDS=6 \
SGATE_STRESS_DURATION=10m SGATE_STRESS_BYTES=1024 \
SGATE_STRESS_CHURN_INTERVAL=6s SGATE_STRESS_OFFLINE_DURATION=100ms \
SGATE_STRESS_NOISE_INTERVAL_MIN=300ms SGATE_STRESS_NOISE_INTERVAL_MAX=700ms \
SGATE_STRESS_REPORT="$PWD/artifacts/wss-combined-long.json" \
go test -run '^TestStressReconnectResources$' -v -count=1 -timeout 16m ./internal/tunnel
```

六轮包含三次重启和两次过期。每个故障阶段另建同等数量的应用连接，阻塞网络并填满两端每个会话的重传缓存，再停止带活跃写协程的服务端，或等待测试保留时间（10 秒）过期。重启会关闭旧 HTTPS 监听器，在原地址用相同证书建立全新的服务端实例。过期时确认服务端先释放旧会话，客户端在收到带认证的会话失效响应前仍保留缓存，响应后清理旧状态。每个阶段等待所有旧应用连接和传输退出，检查其定时器停止，再验证新会话连接。报告的 `intentionally_interrupted_streams` 单独统计这些预期终止的旧流，正常数据流的完整校验字节数不包含它们；旧流使用不同种子的伪随机数据，已送达的数据必须是正确前缀；反例测试覆盖缺字节、重复字节和超长数据，实际故障阶段也拒绝错误前缀及读超时。后续轮次使用新的种子及序号，检查旧缓存没有串入新流。总耗时还包含过期等待、TLS 重连和清理时间；可增加 `-race` 单独运行竞态压力测试。

### 默认两分钟窗口的 WSS 断网测试

`SGATE_STRESS_DEFAULT_RETENTION=1` 不传入服务端保留时间和客户端离线超时的覆盖值，使用两端默认的两分钟。下面在弱网、256 条应用连接、1～3 秒随机间隔和默认 1～10 KiB 填充下，断网 110 秒后恢复：

```sh
SGATE_STRESS=1 SGATE_STRESS_DUPLEX=1 SGATE_STRESS_TLS=1 SGATE_STRESS_WEAKNET=1 \
SGATE_STRESS_DEFAULT_RETENTION=1 SGATE_STRESS_RETENTION_OUTAGE=110s \
SGATE_STRESS_CLIENTS=4 SGATE_STRESS_STREAMS=64 SGATE_STRESS_ROUNDS=1 \
SGATE_STRESS_BYTES=1024 SGATE_STRESS_DURATION=20s SGATE_STRESS_CHURN_INTERVAL=6s \
SGATE_STRESS_NOISE_INTERVAL_MIN=1s SGATE_STRESS_NOISE_INTERVAL_MAX=3s \
SGATE_STRESS_REPORT="$PWD/artifacts/wss-default-retention-resume.json" \
go test -race -run '^TestStressReconnectResources$' -v -count=1 -timeout 5m ./internal/tunnel
```

测试确认断网期间两端每个会话均曾填满重传队列、缓存不超限、白噪音停止发送，恢复后使用原客户端会话、服务端会话和应用连接，双向独立数据的序号、内容、结束标记和 EOF 全部一致。应用及后端的读写截止时间额外包含计划断网时长，计划内断网也不会触发无进展诊断。JSON 的 `retention_outage` 记录特殊断网时长，`retained_session_continuity` 记录会话身份检查结果。

过期后的清理及新连接测试使用下面的组合：

```sh
SGATE_STRESS=1 SGATE_STRESS_DUPLEX=1 SGATE_STRESS_TLS=1 SGATE_STRESS_WEAKNET=1 \
SGATE_STRESS_DEFAULT_RETENTION=1 SGATE_STRESS_LIFECYCLE=1 \
SGATE_STRESS_CLIENTS=4 SGATE_STRESS_STREAMS=64 SGATE_STRESS_ROUNDS=2 \
SGATE_STRESS_BYTES=1024 SGATE_STRESS_DURATION=24s SGATE_STRESS_CHURN_INTERVAL=1m \
SGATE_STRESS_NOISE_INTERVAL_MIN=1s SGATE_STRESS_NOISE_INTERVAL_MAX=3s \
SGATE_STRESS_REPORT="$PWD/artifacts/wss-default-retention-expiry.json" \
go test -race -run '^TestStressReconnectResources$' -v -count=1 -timeout 5m ./internal/tunnel
```

默认窗口模式的生命周期阶段只注入会话过期，阻断网络直至两端所有旧会话和应用连接释放，再恢复网络。客户端自身两分钟超时与服务端回收可能先后发生，不要求二者固定顺序；检查没有提前过期，并在约两分钟后及时释放旧缓存、连接及传输定时器。`expiry-offline-expired` 快照在网络恢复之前记录结果。第二轮用新的独立数据流验证新会话，故障阶段的 256 条旧流仍单独记录为预期终止，不计入完整传输字节数。

### 119 / 120 / 121 秒临界恢复

`SGATE_STRESS_BOUNDARY_OUTAGE` 与 WSS、默认保留窗口、双向独立模式和生命周期模式一起使用，在第一轮正常流之后建立同等数量的旧流、填满双向缓存，再按指定时长恢复网络：

```sh
for seconds in 119 120 121; do
  SGATE_STRESS=1 SGATE_STRESS_DUPLEX=1 SGATE_STRESS_TLS=1 SGATE_STRESS_WEAKNET=1 \
  SGATE_STRESS_DEFAULT_RETENTION=1 SGATE_STRESS_LIFECYCLE=1 \
  SGATE_STRESS_BOUNDARY_OUTAGE="${seconds}s" \
  SGATE_STRESS_CLIENTS=4 SGATE_STRESS_STREAMS=64 SGATE_STRESS_ROUNDS=2 \
  SGATE_STRESS_BYTES=1024 SGATE_STRESS_DURATION=24s SGATE_STRESS_CHURN_INTERVAL=1m \
  SGATE_STRESS_WEAK_DELAY=75ms SGATE_STRESS_WEAK_JITTER=50ms \
  SGATE_STRESS_WEAK_LOSS_PERCENT=15 SGATE_STRESS_WEAK_RETRANSMIT=100ms \
  SGATE_STRESS_NOISE_INTERVAL_MIN=1s SGATE_STRESS_NOISE_INTERVAL_MAX=3s \
  SGATE_STRESS_REPORT="$PWD/artifacts/wss-boundary-${seconds}.json" \
  go test -race -run '^TestStressReconnectResources$' -v -count=1 -timeout 6m ./internal/tunnel
done
```

计时从阻断网络并关闭旧传输开始，实际恢复时间和所有旧流解决后的时间分别记录在 `boundary_restore_milliseconds`、`boundary_resolve_milliseconds`。恢复网络之后仍需完成 TLS/HTTP 握手，因此不会假定 119 秒必然续接、120 秒必然过期。每个客户端单独分类：续接成功须保留原会话，全部旧流双向完整、半关闭及 EOF 正确；过期须释放两端旧会话、缓存、连接和传输定时器，已送达数据只能是正确前缀。任何一侧过期后留下另一侧孤立缓存、后端连接，或保留多余服务端会话，都会失败。第二轮正常流验证新旧流没有串流。报告分别记录 `boundary_resumed_sessions`、`boundary_expired_sessions`、`boundary_complete_old_streams` 和 `boundary_interrupted_old_streams`。

并发数据校验使用保留完整 64 位种子的 PCG 字节流，种子包含轮次、客户端和流编号。反例覆盖旧生成器截取高位种子后发生的跨客户端数据碰撞，并确认分段读取不会改变字节流。

服务端以客户端发来的首个有效加密帧确认续接。升级失败、升级后立即放弃或始终未确认的连接不会刷新旧会话的保留起点；未确认续接的读取及 Ping/Pong 进度也受原保留期限约束。收到有效确认后，连接按正常心跳运行，断开时才开始新的保留窗口。`TestResumeConfirmationControlsRetention` 覆盖放弃、静默和正常确认三种情况。


补充未确认重连的真实两分钟压力测试：在上述默认窗口、WSS、弱网、双向及生命周期配置下，去掉 `SGATE_STRESS_BOUNDARY_OUTAGE`，分别设置 `SGATE_STRESS_UNCONFIRMED=tls-timeout`、`abandoned`、`silent`，使用 64 个客户端、每客户端 4 条流、两轮、24 秒正常负载和六分钟测试超时。前两种在断网开始、中点和原期限前一秒各发起一批探测；静默模式保持升级后的连接，其中一半只读取服务端白噪音并回复 WebSocket Pong，不发送加密协议帧。所有旧会话的保留起点必须始终不变，原期限到达后必须释放两端旧流、缓存和传输定时器，再恢复正常客户端验证新连接。报告记录真实握手超时数、放弃升级数、静默升级数、收到的白噪音数和 `unconfirmed_release_milliseconds`。仅作快速预检时，可设 `SGATE_STRESS_UNCONFIRMED_TTL=6s` 同时缩短测试两端的期限；正式默认窗口测试不设置此项。


### 持续控制帧与缓慢不完整分片

在上一节的未确认重连测试配置下，分别设置 `SGATE_STRESS_UNCONFIRMED=control` 和 `SGATE_STRESS_UNCONFIRMED=fragmented`，使用 64 个客户端、每个 4 条流、两轮正常负载和默认两分钟保留时间。`control` 每 250 毫秒主动发送 Ping、Pong，并读取白噪音及服务端回复；`fragmented` 每 250 毫秒发送一个带掩码的 8 字节 WebSocket 分片，首片为二进制，后续为 continuation，全部不带 FIN。片段来自有效加密消息，但始终缺少后续密文和认证标签，不构成完整确认。两种模式均保持读取、回复服务端 Ping；提前退出探测会导致测试失败。

测试逐一核对旧会话的保留起点不变、缓存不超限、原期限后释放旧连接和传输定时器，并验证恢复后的新会话及独立双向数据。报告增加 `unconfirmed_control_sent`、`unconfirmed_pongs_received`、`unconfirmed_fragments_sent`、`unconfirmed_partial_bytes`。`TestResumeConfirmationControlsRetention` 同时包含这两种短窗口回归，完整压力测试不设置 `SGATE_STRESS_UNCONFIRMED_TTL`。


### 串行排查 macOS 网络内存计数

`netstat -m` 的 `requests for memory denied` 是系统全局的累计网络内存分配失败计数，不能直接作为某个 sgate 进程的泄漏或应用数据丢失计数。先编译普通和竞态测试二进制，再串行比较相同负载，避免将编译及其他压力测试的开销混在一起：

```sh
mkdir -p artifacts
GOMAXPROCS=4 go test -c -o artifacts/network-memory-plain.test ./internal/tunnel
GOMAXPROCS=4 go test -race -c -o artifacts/network-memory-race.test ./internal/tunnel
python3 scripts/diagnose-network-memory.py
```

脚本依次运行持续 Ping/Pong、缓慢不完整分片各自不带和带 `-race` 的四组 WSS 测试，每组 64 个会话、256 条应用流、真实两分钟保留窗口、1～3 秒随机白噪音及模拟弱网；本脚本不并行运行压力测试，也不停止系统现有应用。每组前后各留出 10 秒观测，运行期间每秒记录 `netstat -mmm` 原始输出、每类网络池失败计数、实际占用及已分配容量、测试进程 PID/RSS，并保存阶段资源和完整数据校验结果。输出位于 `artifacts/network-memory-serial/`，可用 `--output`、`--cooldown` 修改目录和前后观测时长。脚本总耗时约 11 分钟。

应比较每组计数增量、实际占用是否回落，以及 Go 堆、协程、描述符、连接、重放缓存和传输定时器是否释放。已分配但空闲的内核池容量、短期 TCP TIME_WAIT 和竞态检测器的 RSS 开销需要单独解释。采样器将有效输出中省略的零失败计数记为 0；平台不支持、读取失败或无法识别输出仍记为不可用。


### 两组压力测试并行时的 mbuf 事件诊断

```sh
GOMAXPROCS=4 go test -c -o artifacts/network-memory-parallel-plain.test ./internal/tunnel
GOMAXPROCS=4 go test -race -c -o artifacts/network-memory-parallel-race.test ./internal/tunnel
python3 scripts/diagnose-network-memory-parallel.py
```

脚本先并行运行两组普通构建，再并行运行两组竞态构建；两种构建之间串行切换。一组持续 Ping/Pong，另一组缓慢发送不完整加密分片，默认错开 2 秒启动。每个进程仍为 64 个会话、4 条流/会话、默认两分钟保留窗口、随机白噪音和相同弱网配置。`--streams 8` 可将两组总应用流数从 512 增至 1024；`--start-offset 0` 可使启动更接近同步。可用 `--output` 指定独立报告目录，避免覆盖其他测试。

每 100 毫秒采集系统全局及各类 mbuf 失败计数，每秒采集指定测试 PID 的 RSS、TCP 描述符/状态、发送和接收队列，以及与已知测试监听端口相关的系统 TCP 状态。`SGATE_STRESS_LIVE_REPORT` 启用原子覆盖的实时阶段快照，包含 Go 堆、协程、连接及双向缓存；断网阶段每秒更新一次。计数变化会保存前后 mbuf 原始输出、最近及即时进程状态、阶段快照、`vm_stat`，并在首个事件保存各测试进程的 `vmmap` 摘要。网络计数、进程状态和 Go 快照并非同一时刻的原子读取，应按各自时间戳关联；不能把采样值当作瞬时峰值。观察器本身也有开销，所列测试 RSS 不包含观察器进程。

每种构建结束后继续观察 35 秒，核对旧流过期、恢复后的完整独立双向数据、连接/重放缓存/传输定时器释放，以及内核网络簇占用和测试相关 TIME_WAIT 是否回落。报告位于 `artifacts/network-memory-parallel/`；系统计数始终是全局值，脚本保留现有应用和系统配置。

观察器的空启动快照与缺失实时监测回归可单独运行：`python3 -B -m unittest discover -s scripts -p 'test_*.py'`。已有目录的汇总会保留未重跑的构建结果，因此可用 `--variants race` 继续尚未完成的竞态对照。

四组并行对照使用独立报告目录，可运行：

```sh
python3 -B scripts/diagnose-network-memory-parallel.py \
  --processes 4 --streams 8 --start-offset 0 \
  --socket-buffer 65536 --churn-interval 4s \
  --reconnect-min 250ms --reconnect-max 10s
```

普通和竞态构建各运行四个进程，构建之间串行切换；每档共 256 个 WSS 会话、2,048 条应用流，仍使用真实两分钟保留窗口、随机白噪音和弱网。两个控制帧场景和两个不完整分片场景使用独立名称及文件，分别验证数据与清理结果。默认输出为 `artifacts/network-memory-parallel-4/`，不会覆盖两组测试记录；继续其中的竞态档时保留相同参数并添加 `--variants race`。首个 mbuf 事件的可选 `vmmap` 采集失败或超时会单独记录，不中断压力测试。

四组诊断可用 `--socket-buffer` 对照测试套接字缓冲（默认 16384，`0` 使用系统默认值），不会修改系统参数或生产程序配置。上述命令用 64 KiB 缓冲及每 4 秒断线重现更高的内核池压力；16 KiB 档需单独报告，不能将其他档的通过结果套用。`--retention-seconds 45` 仅用于缩短故障定位，正式两分钟测试应省略。`--trace` 会额外保存未确认探测连接映射、转发计数，以及断网约 20 秒时的协程栈和 TCP 快照；诊断会增加采样开销。

观察脚本会等待同一档的所有进程完成并保留各进程退出码；某一档失败后仍运行另一构建档，最终以非零退出码及 `failures.json` 报告失败，不把其余测试提前终止或将失败标记为通过。

四组命令显式使用生产默认的 250ms～10s 重连退避；可用 `--reconnect-min 5ms --reconnect-max 25ms` 重现原压力器的激进重试档，并使用独立输出目录。压力器建立全部后端流和等待应用侧关闭通知使用 20 秒预算；超过 5 秒的关闭通知会额外保存应用端地址、TCP 表和协程栈。会话本身的原到期截止及缓存上限断言未放宽，报告分别记录 `unconfirmed_release_milliseconds` 和 `unconfirmed_application_release_milliseconds`，区分代理释放与应用侧观察到关闭的时刻。

纯 TCP 关闭控制（不经过 TLS/WebSocket/代理）可用 `SGATE_STRESS_SOCKET_CLOSE=1 GOMAXPROCS=4 go test ./internal/tunnel -run "^TestStressDirectTCPShutdown$" -count=1 -v` 单独运行，并另加 `-race` 对照。默认先建立 1,024 对连接，再发送大量未被读取的数据，随后关闭对端，检查 5 秒内是否全部收到 EOF 或复位；`SGATE_STRESS_SOCKET_CLOSE_COUNT` 和 `SGATE_STRESS_SOCKET_CLOSE_BUFFER` 可调整连接数及缓冲。该控制会产生较高内核队列压力，不与代理对照混跑。

八组并行对照可使用：

```sh
python3 -B scripts/diagnose-network-memory-parallel.py \
  --processes 8 --streams 8 --start-offset 0 \
  --socket-buffer 65536 --churn-interval 4s \
  --reconnect-min 250ms --reconnect-max 10s \
  --output artifacts/network-memory-parallel-8-defaults
```

每档共 512 个 WSS 会话、4,096 条并发应用流，四组持续 Ping/Pong、四组不完整加密分片，保留真实两分钟窗口、随机白噪音和弱网。两种构建串行运行，结束后各继续观察 35 秒；每组独立保留数据校验、清理断言及退出码。八组默认输出目录为 `artifacts/network-memory-parallel-8/`，不会覆盖四组记录。可选 `vmmap` 现在后台并行采集，并记录实际开始/完成时刻，避免八次串行检查阻塞 mbuf 观察循环；全局池计数与事件现场仍按实际采样时间解释。

未确认重连探测的握手失败会使用该档配置的重连退避重试，所有尝试共用旧会话原始截止时间；超过截止仍未覆盖全部会话会失败。升级成功后的提前断开仍会失败，不会通过重试掩盖。探测控制帧、原始分片和 Ping 回复复用通道配置的写预算，避免 Gorilla 默认一秒回复预算在竞态与弱网压力下造成误报。`unconfirmed_upgrade_retries` 记录握手重试次数，`unconfirmed_probe_write_timeout` 记录实际探测写预算；探测计数在覆盖断言之前保存，因此失败报告也保留实际计数。生产程序的超时、协议和会话保留时间不变。

十六组并行对照可使用：

```sh
python3 -B scripts/diagnose-network-memory-parallel.py \
  --processes 16 --streams 8 --start-offset 0 \
  --socket-buffer 65536 --churn-interval 4s \
  --reconnect-min 250ms --reconnect-max 10s \
  --output artifacts/network-memory-parallel-16-defaults
```

每档共 1,024 个 WSS 会话、8,192 条应用流，八组控制帧探测和八组不完整加密分片探测；两档串行运行，各自保留 16 份日志、报告、退出码及结束后 35 秒观察数据。默认目录为 `artifacts/network-memory-parallel-16/`。连接指标为测试进程持有的 TCP 描述符合计（包含本机两端及中继），内存为这些进程的 RSS 采样合计；mbuf 拒绝计数属于系统累计值，应结合事件现场、实际池占用与负载结束后的变化判断。

单机十六组、每会话八流可能先触及临时端口容量，表现为 `can't assign requested address` 或建连超时，此时不能算完整性测试通过。保留失败目录，再用 `--streams 4 --output artifacts/network-memory-parallel-16-capacity-control` 跑独立容量对照：仍为十六组和 1,024 个会话，共 4,096 条应用流，其余参数保持一致。报告应分别标明两档负载及结果，不把容量对照通过等同于八流档通过。

`--clients` 可设置每组会话数，默认仍为 64。进一步隔离会话规模可使用 `--clients 32 --streams 8 --output artifacts/network-memory-parallel-16-512sessions`，合计 512 个会话、4,096 条应用流，其余参数沿用上面的命令。不同会话数不得复用同一个输出目录；释放时刻超过观察器原定范围时，失败消息会记录实际毫秒值及允许范围。

未确认重连使用独立的原截止定时器强制关闭传输，避免 Ping 回复等待 WebSocket 写锁时越过读超时，延迟旧会话释放。有效加密帧确认与到期通过原子状态竞争：确认成功会停止该定时器，到期胜出则拒绝确认；传输退出也停止定时器。阻塞写回归验证原读超时无法中断的场景，确认后跨越原截止的回归验证健康连接继续工作。

十六组、每组 64 会话、每会话 8 流的单机完整规模对照可使用：

```sh
python3 -B scripts/diagnose-network-memory-parallel.py \
  --processes 16 --clients 64 --streams 8 --start-offset 0 \
  --backend-host ::1 --server-host ::1 --cohort-settle 35s --synchronize-load --tunnel-log \
  --socket-buffer 16384 --churn-interval 4s \
  --reconnect-min 250ms --reconnect-max 10s \
  --output artifacts/network-memory-parallel-16-fullscale/final
```

该配置保留每批 1,024 个 WSS 会话及 8,192 条同时活动的应用流，将测试服务端 HTTP 和后端监听分到 IPv6 回环，客户端及中继入口仍用 IPv4。两批应用连接之间等待 35 秒回收上一批的内核 `TIME_WAIT`；等待只发生在完整批次结束后、下一批开始前，不插入批内断线或两分钟旧会话保留窗口。报告保存 `backend_host`、`server_host`、`cohort_settle`，不同配置不能混用输出目录。观察器统计两种回环地址的临时端口数量及状态，注明所计数范围，并在首次失败时保留 TCP 表、系统内存现场。后端拨号失败会记录实际错误，正常会话取消不额外报错。

数据负载时长从全部后端流就绪后开始，避免较慢的建连吞掉传输时间或断线注入次数。`load_windows` 保存每轮就绪时刻、发送截止和完整流数；近同步启动时，观察器要求所有进程每轮的完整负载窗口存在共同交集，`full_load_overlap_seconds` 记录交集长度。批间回收等待不能替代这个满并发检查。

完整规模命令使用夹具默认的 16KiB 套接字缓冲。限制同时应用于应用端、接受的代理端以及代理主动拨出的后端端点；后者在所有流条目就绪后、发送负载之前设置。`backend_sockets_bound` 检查两个正常批次和一个过期批次均被覆盖。用 64KiB 做单独容量对照时，8,192 流的未读双向负载可能先耗尽本机 mbuf，表现为 `socket: no buffer space available`，即使两种地址族的临时端口尚未用满；应与端口分配失败分开记录，不能将其称为 Go 堆泄漏或将失败档记为完整性通过。

传输的 Ping 处理器只提交有界待回复 Pong，不同步等待 WebSocket 写锁；由原有写协程发送，避免数据写受压时读协程停止处理 ACK，造成双向阻塞。待回复队列只保留最新负载，符合 [RFC 6455 §5.5.3](https://www.rfc-editor.org/rfc/rfc6455.html#section-5.5.3)，不进入可靠重放队列。定向回归先阻塞数据写，再发 Ping 和有效加密 ACK，要求写仍阻塞时 ACK 已释放缓存，并在写恢复后收到原样 Pong。

弱网夹具只合并发送时刻已到的相邻分段，不越过未来分段；存在发送间隔限制时不合并。无谓的已过期计时等待被跳过，`relay_coalesced_packets`、`relay_writes`、`relay_wait_timers` 记录实际执行。基线要等客户端、服务端及全部传输写协程就绪；过期前缀校验仍检查每个收到的字节，但不额外合成未收到的尾部。

纯系统端口对照可单独运行 `python3 -B scripts/diagnose-port-capacity.py --output artifacts/tcp-port-capacity.json`，不要与其他压力档混跑。它不加载 sgate、不发应用数据，逐步持有 IPv4 TCP 连接，检查新 IPv4 目标与 IPv6 的分配结果，并保存 mbuf 前后差值的原始表。只提高诊断进程自身的 FD 软限额，不修改系统临时端口或网络内存设置。

保持十六组、每组 64 会话和 8 流的内存容量对照可将上述完整规模命令的 `--socket-buffer` 改为 `8192`，并使用独立目录 `artifacts/network-memory-parallel-16-fullscale/bounded-8k`。该值仅约束测试端点缓冲，不改变生产配置、会话队列或并发规模，也不保证消除系统 mbuf 压力；尤其未约束的传输端点仍使用系统默认缓冲。16 KiB 与 8 KiB 档必须分别报告，不能据其中一档推定另一档通过。

传输退出错误同时保留写端原始错误和读端错误，避免写超时关闭套接字后只看到“连接已关闭”。未确认探测失败记录中继首个关闭原因、方向及转发字节数，默认不启用逐包日志。压力测试失败路径也在清理后采样并保存 `closed` 资源状态；退出码仍为失败，清理成功不能替代完整性通过。

客户端 TCP 入口统一使用本地扩展的 `go-service` 模板：`TcpServiceInit → AcceptHandler/CloseHandler → Run`，独立启动与 acl_master 托管共用生命周期。管理 HTTP/HTTPS 端口由独立协程监听；保存应用代理配置不会重建管理监听或清除登录会话。TCP 回调在应用流关闭后才返回，使框架连接计数覆盖完整连接生命周期，并保留 TCP 半关闭能力。独立模式支持端口热修改及失败回滚，托管模式的业务端口由 acl_master 持有，修改后需通过 master_ctl 重启。运行状态提供 `tcp_framework=go-service`、当前连接数 `tcp_connections` 和累计关闭连接数 `tcp_closed_connections`。

`third_party/go-service` 基于 v1.1.2 增加可取消的运行上下文、等待退出和可重启代理工作实例；通过 go.mod 的本地 replace 使用，标准 vendor 由 `scripts/sync-go-vendor.py` 同步。参考示例 `vendor/go-service/examples/go-echod` 保持原样。

客户端支持用一个进程合并多个 TCP 代理。在 `sgate-client.cf` 的 service 块中配置：

```ini
proxies = '[{"listen":"127.0.0.1:9001","server":"wss://gateway.example.com:8081/tunnel?addr=127.0.0.1:22"},{"listen":"127.0.0.1:9002","server":"wss://gateway.example.com:8081/tunnel?addr=backend.example.com:8088"}]'
```

`proxies` 留空时保留原 `listen/server/addr` 单代理行为；非空时使用数组中的监听地址和远程 URL，顶层 `listen/server/addr` 不参与映射。每项必填 `listen` 和 `server`，可选 `addr` 覆盖该项 URL 中的地址。每个本地端口必须唯一且不能为 0，最多 64 项。远程地址仍须在服务端 `allowed-targets` 中；未携带 addr 时使用服务端默认后端。所有映射共用顶层密钥、密码、CA、心跳、重连和随机白噪音设置；`send-addr=false` 对所有映射生效。每个映射拥有独立长连接、会话和有界重放缓存，`max-streams/buffer-bytes` 按映射分别限制，管理界面带宽为整个进程汇总。

命令行 `-proxies '<JSON数组>'` 覆盖文件中的整个列表，`-proxies ''` 切回单代理设置。独立启动会由 go-service 同时监听所有端口；托管模式还需将 `master_service` 设为相同端口，例如 `127.0.0.1|9001,127.0.0.1|9002`。启动时逐项核对继承端口，缺失、多余或重复都拒绝启动。管理界面可保存并应用映射；应用会关闭原业务连接并重建各代理，登录会话保持有效。独立模式支持修改监听端口及绑定失败回滚；托管模式修改端口须保存 `.cf` 后通过 master_ctl 重启，原端口上的远程 URL/addr 修改可热应用。

多代理集成回归脚本为 `scripts/test-client-multi-proxy.py`，需提供 `build-config-soak.py --managed --race` 生成的诊断二进制。覆盖独立/托管、WS/WSS、同一客户端进程三个监听、128 并发、随机白噪音、频繁断线、Web 配置映射热切换、失败回滚、连续重启及登录会话和资源释放。

多端口代理的默认两分钟保留回归使用 `scripts/test-client-multi-retention.py --client <诊断客户端> --server <诊断服务端> --mode standalone --tls wss --output <新目录>`；托管模式改为 `--mode managed`，WS 改为 `--tls ws`。诊断二进制由 `scripts/build-config-soak.py --managed --race --watch-goroutines --check-registry` 构建。测试不写入 `session-ttl` 或 `offline-timeout`，直接核对运行默认值 120 秒；同一客户端进程的三个端口承载 128 并发，双端开启随机白噪音。断网至少 75 秒后要求原连接双向完整续传；再次断网超过 125 秒，检查旧连接、后端连接和缓存自动清空、新连接传输正确；随后在断网中重启服务端并改变默认后端，验证默认端口切换到新后端而显式地址端口保持原选择，旧缓存不能进入新流。超时及异常时保存协程栈、连接和缓存状态。客户端进程全程不重启，避免掩盖资源泄漏。

客户端管理页的 `proxies` 使用映射表单显示，每行分别编辑本地监听地址、远程 WebSocket 地址和可选后端地址，支持新增、删除及修改后保存；原 URL 中的 `addr` 会分离展示。映射表提交结构化数组，由后端校验、编码并写回配置文件，保留其他配置、注释和密码。尚无 `proxies` 项的旧配置也可在界面首次添加。删除全部映射会保存为空值并回到单代理配置；托管模式改变监听端口仍须同步 `master_service` 并重启服务。修复了映射 JSON 经过双引号转义后膨胀、超过模板 4095 字节行长而导致保存校验失败的问题，优先保留紧凑的单引号 JSON 编码。


映射表单生命周期回归可使用 `scripts/test-mapping-form-lifecycle.py`，传入 `build-config-soak.py --managed --race --watch-goroutines --check-registry` 生成的客户端和服务端二进制，以及 `--mode standalone|managed --tls ws|wss --output <空目录>`。每组保持 128 并发，覆盖新增、修改、删除、清空、连续保存与应用、CLI 覆盖、校验及绑定失败回滚、旧 Cookie 跨进程恢复。前端草稿和密码保留另由 `node --test scripts/test-config-form.cjs scripts/test-admin-restart.cjs` 校验。

客户端托管模式修改端口后，须同时保存对应的 `master_service`；Web 整服务重启会检测端口集合变化，依次调用 `master_ctl stop`、等待旧进程退出、再调用 `master_ctl start`，让框架重建监听描述符。端口集合不变时继续使用 `master_ctl restart`。整个流程在独立控制进程中执行，旧代理退出不会取消启动步骤。代理及进程重启会关闭现有业务 TCP 连接；测试分别检查重启前收到的数据前缀与重启后的新连接，隧道断线恢复阶段则完整核验双向记录、序号和结束标记。


白噪音的最大单次总量支持 `10M`，两端默认仍为 `10240` 字节。例如：

```ini
noise-interval = 0
noise-interval-min = 1s
noise-interval-max = 3s
noise-min-bytes = 1K
noise-max-bytes = 10M
```

固定等待使用 `noise-interval = 1s`，同时将随机间隔 min/max 设为 `0`。随机等待须同时填写 min/max，并将固定间隔设为 `0`；值使用 `ms`、`s`、`m` 等时间单位。在 Web 管理页保存这些白噪音参数时会同步写入文件并立即更新现有隧道，无需重启；直接从外部编辑文件仍需通过界面应用或重启。命令行同名参数仍优先。配置页“本端白噪音”显示当前运行参数，噪音负载累计分别显示本端发送和接收，便于核对实际效果。只有客户端已经建立隧道长连接时才会发送噪音；服务端关闭发送不影响客户端独立发送的噪音或正常心跳。

大小超过 16 KiB 时，一次随机总量分块发送，而非构造一个 10 MiB 的 WebSocket 消息；内存保持按小帧分配，旧版本接收端仍可正常解密并丢弃这些小帧。每次总量发送完成后再等待下一个间隔，网络阻塞可能拉长实际间隔，不追补积压噪音。

服务端噪音配置的隔离集成回归可使用 `scripts/test-server-noise.py`，传入诊断二进制的 `--client`、`--server`，并指定 `--mode standalone|managed --tls ws|wss --output <空目录>`。脚本仅在临时服务上运行，验证固定/随机间隔、保存后无重启生效、K/M 大小及 CLI 优先级、关闭和超限回滚，叠加三个客户端映射、128 并发及频繁断线；每次切换在线噪音配置后核对运行值和数据完整性。

管理配置表单会在每个有量纲的数字旁注明单位、范围、示例和零值含义：时间支持 `ms/s/m/h`，框架的整数秒字段不带后缀，白噪音大小支持 `K/M`，缓存大小只填字节整数。输入时显示字段错误，保存前检查类型、范围、地址、映射重复和关联约束；非法值不会发送保存请求，已输入草稿保留。相关参数需要一起修改时使用整页保存，例如同时调整随机间隔上下限或证书和私钥。单项保存只校验该项与其他已保存值，不受其他行未保存草稿影响。后端重复执行类型及应用配置校验，文件存在性、权限和证书内容仍由后端检查。

字段保存审计可运行 `scripts/test-config-field-save.py --client <诊断客户端> --server <诊断服务端> --mode standalone|managed --tls ws|wss --output <空目录>`。脚本在隔离服务上逐项保存并恢复客户端和服务端配置，验证非法值回滚和登录保留，再以 128 并发核验双向数据和断线恢复。字段覆盖与单位规则另由 `internal/admin/config_rules_test.go` 检查，前端拦截由 `scripts/test-config-form.cjs` 检查。


### 保存后立即生效的参数

客户端和服务端：`noise-interval`、`noise-interval-min`、`noise-interval-max`、`noise-min-bytes`、`noise-max-bytes`、`noise-traffic`、`noise-idle`、`noise-empty`、`noise-idle-after`。
客户端另支持：`reconnect-min`、`reconnect-max`。

管理页为这些字段显示“保存后立即生效”。行内保存和整页保存都会先校验、备份并写入配置文件，再原子更新运行参数；失败则恢复原文件。仅涉及在线参数时，“保存并应用”也不会重启代理。上下限等关联项需整页一起保存，防止中间状态无效。命令行已覆盖的字段会额外标注，文件值可以保存，但运行值仍遵守命令行优先级。

噪音启停、间隔或场景策略变更唤醒当前等待，重新按新间隔计时；仅修改大小用于新的一次噪音发送，不推迟既定发送时间，也不中断当前批次。重连参数修改不重置噪音时钟。关闭时丢弃未发送的噪音余量，已开始写出的小帧允许完成。每个隧道只保留一个噪音定时器。重连间隔变化会唤醒当前退避等待，并使用新范围；仅修改噪音不会重置重连退避。现有业务 TCP、WebSocket、序列号和重放缓存不因此重建，离线会话保留期限不因此延长。

其余参数仍走原应用/重启流程：后端选择、路径、密钥、认证、TLS、缓存/流数量、心跳与超时、会话保留等需要应用代理配置；框架监听与管理账号/会话/界面设置需要重启整个进程。心跳和保留期限涉及当前读超时与未确认重连的截止时间，本次不将它们作为无损即时参数。混合保存时只更新在线字段，其余继续显示待应用，不会因一次噪音保存提前切换后端。

隔离验证脚本为 `scripts/test-live-config.py`，参数与 `test-server-noise.py` 相同；覆盖独立/acl_master、WS/WSS、128 并发、随机噪音、隧道断线、连续在线保存、CLI 覆盖和关闭后的资源释放。

连续在线修改压力测试可给 `scripts/test-live-config.py` 添加 `--live-waves 6 --records 384`：同一对客户端/服务端进程连续运行，6 轮修改与 1 轮关闭噪音后的验证均保持 128 并发，三个映射核对不同后端；每 650ms 切断隧道，并循环改变固定/随机/关闭模式、噪音大小和重连上下限。每次保存重新读取文件并核对运行值、命令行覆盖、PID 和代理代次；每轮业务结束采集协程、堆、RSS、文件描述符、连接和缓存，异常监视器保留协程栈。

`build-config-soak.py` 生成的诊断 overlay 另计数噪音、心跳和重连定时器的持有数量（不是 Go 运行时所有定时器数量）。单进程三个隧道时，各计数不得超过 3，进程关闭采样须归零；关闭发送后的噪音定时器已停止，仍由对应写协程持有，到写协程退出时释放。观察代码和计数不进入生产二进制。


即时参数连续压力夹具默认等128路全部就绪后再开始完整双向传输，屏障前仅发送首条路由确认记录，减少测试接收者尚未启动时的额外socket积压；`--preload-downstream` 保留旧预发送档用于容量诊断。各后端记录波次与连接编号，拒绝相同业务流重复建连。并行多组会进一步消耗系统网络容量；本机出现过4×128并行ENOBUFS及完整性失败，详细记录见 `artifacts/live-config-continuous/`，该失败没有计为通过。常规四种部署/协议组合按每组128并发串行验证。


白噪音按每条 WebSocket 长连接发送，多端口映射各自独立；同一长连接中的多个业务 TCP 流共用噪音。客户端和服务端独立配置发送，启用其中一端只增加该方向的噪音。噪音与业务共用加密通道，解密后按 `opNoise` 识别并丢弃，不进入后端 TCP 或可靠重放缓存。默认业务繁忙不会停掉噪音；可用场景开关控制是否发送。写协程在业务帧之间检查噪音，每次最多写 16 KiB 噪音块，再返回处理业务及心跳。链路拥塞、调度延迟以及大批次的写入耗时可能使实际间隔增长，这不是固定带宽填充机制。

逐连接线缆帧回归可运行 `go test -race ./internal/tunnel -run '^TestNoise(NotPostponed|MixingPer)' -count=1 -v`。设置 `SGATE_NOISE_OBSERVATION_DIR` 可导出每条连接、每个方向在业务繁忙、空闲及业务流关闭阶段的噪音帧、业务帧、字节数及最大观测间隔。测试仅用临时密钥解密观察原样转发的 WS/WSS 密文，并验证实际双向回显与流/缓存释放。


### 按业务场景控制白噪音

客户端与服务端均支持以下参数，配置文件、命令行、在线表单名称一致；命令行显式设置优先。三个开关默认均为 `true`，旧配置行为保持不变。总发送开关仍为固定/随机间隔：`noise-interval` 及随机 min/max 都为 `0` 时，任何场景均不发。

| 参数 | 默认 | 含义 |
| --- | --- | --- |
| `noise-traffic` | `true` | 有业务连接且双向业务数据仍活跃时混入噪音；设 `false` 暂停此场景 |
| `noise-idle` | `true` | 有业务连接但双向业务数据已空闲时发送噪音 |
| `noise-empty` | `true` | 隧道在线但没有业务连接时发送噪音 |
| `noise-idle-after` | `1s` | 双向无业务数据持续此时间后算空闲；支持 `ms/s/m/h`，`0` 使用 `1s`，不允许负数 |

状态按每条 WebSocket 内所有业务 TCP 流合并判断，各映射独立；任一方向业务活跃即属传输中。新建且从未传输数据的业务连接直接视为空闲。ACK、Ping/Pong、噪音自身均不算业务数据，持续噪音不会阻止进入空闲状态。断开网络后没有新的噪音传输，重连沿用业务连接及其活动状态，关闭最后一条业务连接后切到无连接策略。

例如只在业务间隙发送（无业务连接时也不发送）：

```ini
noise-traffic = false
noise-idle = true
noise-empty = false
noise-idle-after = 100ms
noise-interval = 0
noise-interval-min = 1s
noise-interval-max = 3s
noise-min-bytes = 1K
noise-max-bytes = 10K
```

该示例在连续无双向业务数据达到 100ms 后，按随机 1～3s 的候选发送时间在间隙中发噪音；若业务连接安静 10s，噪音可在这段时间内多次发送。固定等待改为 `noise-interval = 2s` 并将随机 min/max 均置 `0`。`noise-idle-after = 10s` 表示先等无流量满 10s 才进入空闲，和“在前 10s 间隙中发送”不同。

场景筛选发生在每次候选发送及每个噪音小块之前。禁止的场景跳过该次发送，重新选择下一次固定/随机等待；恢复允许的场景后在下一次候选时间发送，不补发积压。三个场景全部关闭时停止噪音定时器，心跳继续。在线修改场景开关或空闲阈值会取消待发余量并重新计时；已经写出的一个小帧允许完成。共用链路拥塞、系统调度与批次写入耗时仍会增加实际间隔，不保证实时固定带宽。

这四项均标注“保存后立即生效”，保存同步到配置文件；配置页的本端噪音摘要显示实际运行场景策略和空闲阈值。外部编辑文件仍需应用代理配置或重启才能加载。两份正式配置及 `samples/` 内五份示例均已补齐参数和中文注释，既有地址、密码、证书及噪音间隔/大小保持不变。


### 判断流量空白期是否真的停发噪音

概览图可以切换“总流量 / 白噪音负载 / 业务负载”，三者各自计算纵轴。总流量曾出现 MiB/s 峰值时，数 B/s 的后台业务或 KiB/s 的噪音可能贴近零轴；图表很低不等于没有字节。概览页另显示噪音收发实时速率、累计字节、业务负载实时速率，以及每条在线隧道汇总的忙碌、空闲、无业务连接数量和暂停原因。

`noise-traffic=false` 时，每个业务字节都会刷新 `noise-idle-after` 窗口。例如阈值 1s、每 250ms 传一个字节，就始终属于业务活跃，会暂停噪音；这段后台数据在大带宽图中可能看似空白。真正停止业务数据达到阈值后，才按原配置间隔恢复发送。希望在更短的间隙发噪音可在线降低阈值，例如 100ms；希望少量业务期间也持续混入噪音则开启 `noise-traffic`。这两种选择由使用者决定，不会因图表显示很低而自动更改策略。

统计 API 的 `business_tx_bytes/business_rx_bytes` 是实际认证加密帧内业务负载字节，`noise_tx_bytes/noise_rx_bytes` 是噪音负载字节，均不含协议开销且包含实际通道重发；总 TCP 字节仍含协议和管理开销。`noise_activity` 仅包含当前传输的状态数量及策略暂停原因，不暴露会话标识、地址或业务内容。传输写协程退出时注销观测引用，不保留过期会话；此功能没有增加后台协程或定时器。


流量图支持鼠标悬停：移入绘图区即可查看最近采样点的本地时间、发送和接收平均速率，并高亮两条曲线上的对应点。总流量、白噪音和业务负载视图使用各自数值；零值显示 0 B/s，未提供的统计不会伪装为零。移出图表、窗口失焦或退出登录后提示消失，实时刷新时提示随鼠标位置更新。此功能仅涉及独立前端文件，更新 Web 目录内容后刷新页面即可，无需重启代理。


流量图横轴使用实际采样时间，显示最近60秒，每5秒一个刻度；后台采样变慢时不再把采样点等距排列成错误的时间跨度。鼠标提示仍显示该点的实际时间和收发速率。

业务应用暂停读取（例如播放器预读后等待播放）时，隧道读者继续消费噪音、ACK和心跳，业务数据交给会话内的有界队列按原顺序写入TCP。只有成功交付后才推进ACK；已入队但未确认的数据跨重连保留，并按序号去重。每方向缓存仍受 `buffer-bytes` 限制；会话过期或关闭时清空队列并关闭业务TCP，以中断阻塞的交付。业务TCP不再套用隧道的15秒写超时，避免正常的应用背压触发RESET。

新版两端通过已认证的握手协商接收窗口：客户端自动添加保留查询参数 `sgate-window`，该参数包含在请求认证中；服务端的 `X-Sgate-Receive-Window` 纳入响应认证。发送端按对端窗口限制未确认的业务数据，避免接收队列满后再次阻塞隧道读者；噪音不占业务窗口，也不进入重放或交付队列。无需手动配置窗口参数。旧端仍可使用原握手，但双方都升级后才具备完整的背压保护。

监控同时显示离线保留的业务连接、业务待交付缓存，以及最近的隧道断开时间和原因；诊断只保留一个有长度上限的错误，避免频繁断线积累日志对象。时间轴更新只需刷新页面；交付队列和窗口协商需要更新并重启两端程序。
