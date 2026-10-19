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

服务端：

```sh
./bin/sgate-server \
  -listen :8080 \
  -target 127.0.0.1:5432 \
  -key-file sgate.key
```

客户端：

```sh
./bin/sgate-client \
  -listen 127.0.0.1:15432 \
  -server ws://SERVER_IP:8080/tunnel \
  -key-file sgate.key
```

应用客户端连接 `127.0.0.1:15432` 即可。服务端的目标地址固定在服务端配置中，客户端不指定任意转发目标。多个客户端进程可以共享同一个服务端，各自建立独立会话。

也可以在两端用 `SGATE_KEY` 环境变量传入 64 位十六进制密钥；指定 `-key-file` 时优先使用文件内容。

### TLS

对外部署建议使用 `wss://`，同时保护 HTTP 握手信息。使用受客户端信任的证书和与证书匹配的域名：

```sh
./bin/sgate-server \
  -listen :8443 -target 127.0.0.1:5432 -key-file sgate.key \
  -tls-cert server.crt -tls-key server.key

./bin/sgate-client \
  -listen 127.0.0.1:15432 \
  -server wss://gateway.example.com:8443/tunnel \
  -key-file sgate.key
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
| `-max-streams` | `128` | 每个会话允许的 TCP 流数 |
| `-buffer-bytes` | `4194304` | 每个方向未确认帧的缓存上限，至少 `16409` 字节 |

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
| `-tls-ca` | 无 | 额外信任的 PEM CA 证书 |

## 断线恢复的边界

“应用无感”指短暂的 WebSocket 中断期间，两端保留原有 TCP 套接字，并在恢复后继续按序传输，不要求应用重新连接。中断时应用读写可能暂时阻塞。

- 默认在服务端检测到连接断开后保留会话 2 分钟，可用 `-session-ttl` 调整。
- 应用和目标服务器必须仍保持原有 TCP 连接；应用自己的请求/空闲超时仍然生效。
- 会话及未确认数据保存在内存中。任一进程重启、会话过期或目标 TCP 断开，都无法恢复原有 TCP 流。会话丢失时客户端关闭旧连接，并建立新会话供后续 TCP 连接使用。
- 确认表示数据已经写入目标 TCP 套接字或已处理对应控制帧，并不表示目标业务完成了事务。
- 默认每 20 秒发送心跳，60 秒内无接收活动或 Pong 则检测为掉线；重连时若旧传输尚未退出，服务端会暂时拒绝重复接入，客户端继续重试。
- 目标连接超时为 10 秒，单次 TCP/WebSocket 写超时为 15 秒。慢目标可能使同一会话的其他流短暂等待。
- 标准 HTTP/WebSocket 行为允许正常的服务器和反向代理处理流量；加密及 HTTP 页面不保证流量不可识别。`ws://` 的握手头部仍可见。

## 协议与实现

每个 WebSocket 二进制消息包含一个加密帧。明文头为 `op(1) | seq(8) | ack(8) | stream(8)`，使用大端编码，数据块最多 16 KiB。操作包括 `ACK`、`OPEN`、`DATA`、`FIN`、`RESET`。每个方向使用独立、跨重连连续的可靠帧序号，纯 ACK 不占可靠序号；重新连接后重传未确认帧，接收方根据序号去重。

加密层每次传输从计数器 1 开始，计数器构造 GCM nonce，并严格检查顺序。会话 ID、双方随机数及方向参与派生加密密钥，因此不同传输不会重复使用同一密钥与 nonce。握手签名绑定协议版本、会话 ID、随机数、时间戳、恢复标志及请求 URI。

核心代码在 `internal/tunnel`；入口在 `cmd/sgate-client` 和 `cmd/sgate-server`。测试覆盖数据篡改和重放、错误密钥、多路复用、半关闭、大数据恢复、丢失确认后的去重、缓存背压、会话过期及目标不可用。
