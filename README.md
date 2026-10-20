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

## 构建与测试

需要 Go 1.23 或以上版本。

```sh
go mod download
make build
make test
```

或手动构建：

```sh
mkdir -p bin
go build -o bin/sgate-server ./cmd/sgate-server
go build -o bin/sgate-client ./cmd/sgate-client
go test -race ./...
```

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
| `-key-file` | 无 | 密钥文件；未指定时读取 `SGATE_KEY` |
| `-password` | 必填 | 连接验证密码，两端保持一致，1～1024 字节 |
| `-max-streams` | `128` | 每个会话允许的 TCP 流数 |
| `-buffer-bytes` | `4194304` | 每个方向未确认帧的缓存上限，至少 `16409` 字节 |
| `-heartbeat` | `20s` | WebSocket 心跳间隔 |
| `-heartbeat-timeout` | `0` | 接收空闲超时；`0` 表示心跳间隔的三倍，默认 `60s` |

服务端参数：

| 参数 | 默认值 | 含义 |
| --- | --- | --- |
| `-listen` | `:8080` | HTTP/HTTPS 监听地址 |
| `-target` | 必填 | 应用服务器的 `host:port` |
| `-path` | `/tunnel` | WebSocket 路径 |
| `-session-ttl` | `2m` | 传输连接断开后保留会话的时间 |
| `-max-sessions` | `128` | 活跃及保留会话总数上限 |
| `-tls-cert` / `-tls-key` | 无 | HTTPS 证书及私钥，必须同时提供 |

客户端参数：

| 参数 | 默认值 | 含义 |
| --- | --- | --- |
| `-listen` | `127.0.0.1:9000` | 应用客户端连接的本地 TCP 地址 |
| `-server` | 必填 | 完整 `ws://` 或 `wss://` URL |
| `-reconnect-min` | `250ms` | 初始重连退避上限，实际等待为其 50%～100% |
| `-reconnect-max` | `10s` | 最大重连退避上限 |
| `-offline-timeout` | `2m` | 客户端持续离线时保留应用连接和未确认数据的时限 |
| `-tls-ca` | 无 | 额外信任的 PEM CA 证书 |

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

每个 WebSocket 二进制消息包含一个加密帧。明文头为 `op(1) | seq(8) | ack(8) | stream(8)`，使用大端编码，数据块最多 16 KiB。操作包括 `ACK`、`OPEN`、`DATA`、`FIN`、`RESET`。每个方向使用独立、跨重连连续的可靠帧序号，纯 ACK 不占可靠序号；重新连接后重传未确认帧，接收方根据序号去重。

加密层每次传输从计数器 1 开始，计数器构造 GCM nonce，并严格检查顺序。会话 ID、双方随机数及方向参与派生加密密钥，因此不同传输不会重复使用同一密钥与 nonce。握手签名绑定协议版本、会话 ID、随机数、时间戳、恢复标志及请求 URI。

核心代码在 `internal/tunnel`；入口在 `cmd/sgate-client` 和 `cmd/sgate-server`。测试覆盖数据篡改和重放、错误密钥、多路复用、半关闭、大数据恢复、上下行丢失确认后的去重、双向独立数据连续断线 8 次、缓存背压、流重置解除等待、取消后的连接释放、会话过期及目标不可用。确认队列清空后释放底层数组，流重置时唤醒等待缓存的生产者。

恢复场景回归还包括：断网期间双向缓存满、客户端主动离线过期、静默丢包后心跳超时、HTTP 握手卡住、服务端在同一端口重启、连续五次重启，以及恢复后新连接不接收旧会话的残留数据。可以按真实默认两分钟窗口执行长断网用例：

```sh
SGATE_RECOVERY_LONG=1 go test -race \
  -run '^TestLongRecoveryDefaultOfflineWindow$' -v -timeout 3m ./internal/tunnel
```

同一流的读取与写入失败可能并发上报，RESET 按流去重，避免重复重置挤满控制队列并终止整个会话。

## 资源压力测试

```sh
make stress

# 持续三分钟，跨过两分钟的握手防重放缓存保留窗口
SGATE_STRESS_DURATION=3m GOMAXPROCS=4 make stress

# 为压力场景开启竞态检测
SGATE_STRESS=1 GOMAXPROCS=4 go test -race \
  -run '^TestStressReconnectResources$' -v -timeout 10m ./internal/tunnel
```

默认使用 4 个客户端，每端 64 路 TCP 流，共 256 路并发连接，重复 12 轮；每次交换 128 KiB 数据，每 30 ms 注入一次传输断线，模拟 10 ms 网络不可用。每轮校验完整回传数据和 EOF，然后确认连接及未确认队列已释放。设置持续时间后，同一批连接反复交换数据直至该轮时间结束，再半关闭和周转连接。

可通过 `SGATE_STRESS_CLIENTS`、`SGATE_STRESS_STREAMS`、`SGATE_STRESS_ROUNDS`、`SGATE_STRESS_BYTES` 和 `SGATE_STRESS_DURATION` 调整负载。`make stress` 默认写入 `artifacts/stress.json`，可用 `SGATE_STRESS_REPORT` 指定报告路径；直接调用 `go test` 时也需显式设置该变量才保存 JSON。

报告记录活跃期和空闲期的堆内存、栈、协程、实际文件描述符、TCP 连接数、会话数、重传缓存和鉴权随机数缓存数量；空闲采样执行 GC，并记录 RSS。macOS 的文件描述符计数使用 `lsof`，RSS 使用 `ps`。Go 运行时保留的页和竞态检测器的影子内存会计入 RSS；通过多轮空闲堆和 RSS 趋势及连接、协程回落情况判断是否持续增长。防重放缓存按两分钟窗口保留，关闭服务端时清空。测试会断言每轮资源回落，并检查最终关闭后的连接及协程释放。

## 弱网回归测试

```sh
make weaknet
```

使用本机有界 TCP 中继注入单向 150ms 延迟、±100ms 抖动和 15% 分段重传等待（200ms），并验证空闲心跳、并发随机数据、慢速分段接收、双方向短时阻塞、单向长断网后的重连和会话过期释放。中继以每段最多 512 字节、有界队列调度，保持 TCP 字节顺序；丢包用延迟重传来模拟，不会把删除 TCP 流字节误当作 IP 丢包，也不修改系统网卡或内核网络规则。

每轮校验随机数据逐字节一致、后端收取字节总数和半关闭后的 EOF；在保留窗口内继续使用同一应用 TCP 连接，超过窗口则释放旧连接和重传缓存，恢复后新连接正常通信。上行中断时，下行已送达的字节可能仍在应用的 TCP 接收缓冲中，测试先读取这一合法前缀，再确认 EOF 或连接重置。清理阶段检查中继连接、排队字节、应用连接、会话缓存和协程均正常释放。
