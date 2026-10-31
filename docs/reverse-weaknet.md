# 反向代理弱网验证与半关闭日志排查

## 日志的含义

`stream=9223372036854775812` 等 ID 大于 `2^63`，属于服务端发起的反向业务流。`TCP half-close failed ... shutdown: transport endpoint is not connected` 表示在该流上执行 TCP 写方向半关闭时，操作系统返回 `ENOTCONN`：对应业务套接字已经不在连接状态。

错误地址中的 `服务端反向监听端口 -> 外部访问者端口` 标识失败发生在反向入口 TCP，而非必然发生在 WebSocket 连接上。外部应用提前关闭、RST、NAT/网络状态丢失，或延迟到达的 FIN 与本地关闭发生竞争，都可能与此现象相关；仅凭该日志不能确定具体原因。不能仅根据连接建立与报错相隔约两分钟，就断定是会话保留期限导致：在线会话不会因 `session-ttl` 到期而主动结束。

现有处理路径为 `FIN → CloseWrite → failStream → 释放该流并发送可靠 RESET`。不会为掩盖错误而忽略半关闭失败，也不会因为单个业务套接字的 `ENOTCONN` 主动关闭整条 WebSocket。若另外触发 RESET 控制队列溢出等资源保护，仍可能结束会话，并记录 `session closed: reset control queue limit reached` 等原因；排障时应一并检查。

本次改进保留此行为，新增每流目标记录，避免反向流错误沿用会话的正向 `target`。新的错误条目包含：

```text
session=<会话ID> account="test" target="<会话正向目标>"
stream=<高位反向流ID> flow=reverse stream_target="<实际客户端目标>"
local="<该端业务套接字本地地址>" peer="<该端业务套接字对端地址>"
TCP half-close failed: <原始系统错误>
```

实际日志输出为一行；其中前缀 `target` 仍是会话绑定的正向目标，`stream_target` 才是当前反向流的实际目标。定位时还应关联同会话的 `tunnel disconnected`、重连记录，以及对端相同流 ID 的 RESET/读写错误；不要把另一个客户端临时探测连接的 `gin-service closed` 自动归因到当前隧道。

## 验证方式与覆盖

测试实现：[reverse_weaknet_test.go](../internal/tunnel/reverse_weaknet_test.go)。使用真实 TCP、真实 WS/WSS、现有客户端自动重连循环及真实 TLS 端点；没有用手动替换 session 来代替恢复。

弱网夹具是有序 TCP 字节中继。它按 512 字节合成分段模拟延迟、抖动、带宽/队列停顿和重传等待，保留原始字节顺序；所谓“15% 重传”是对相应合成分段额外延迟，**不是删除 TCP 字节**，也不是操作系统级丢包实测。该夹具不能证明某条公网链路或 NAT 的真实故障原因。

| 场景 | 参数/动作 | 必须满足的结果 |
| --- | --- | --- |
| 高延迟、抖动、重传等待 | 单向 150±100 ms；15% 合成分段额外等待 200 ms；4 条反向流同时传输 | 请求及响应逐字节一致，目标累计输入字节精确相等，无误重连或目标重建 |
| 短时单向断流 | 分别暂停隧道上行、下行 250 ms，叠加 20±10 ms 延迟与重传等待 | 另一方向仍消费心跳/噪音，恢复后原流继续，不触发误重连 |
| 多轮单向长断流 | 暂停超过 900 ms 心跳阈值，交替上行与下行，等待客户端实际发起重连后放行 | 保留原 session、原目标 TCP，不重复或丢失字节，缓存有界 |
| 与现场相同的半关闭错误码 | 反向入口测试连接的 `CloseWrite` 明确返回包装后的 `syscall.ENOTCONN` | 只产生一次该流半关闭错误并发送 RESET；既有健康流与新流仍可传输，隧道不重连 |
| 外部真实 RST 与迟到 FIN | 外部先半关闭，再对真实 TCP `SetLinger(0)` 并关闭，叠加弱网延迟 | 该业务流释放，健康流与后续新流不接收残留字节；隧道继续使用 |
| 同账号双隧道 | 账号均为 `test`，会话正向目标分别为 `127.0.0.1:2900`、`127.0.0.1:1982`；一条隧道单向断流 | 已有反向流仍在原会话续传；新反向连接可经另一条已确认在线隧道接入，两条流数据互不混入 |

延迟与短断流、长断流和真实 RST 场景均覆盖 WS/WSS，以及反向入口和客户端目标均为普通 TCP/均启用 TLS 两种组合。精确的 `ENOTCONN` 注入覆盖 WS/WSS 的 TCP 入口；TLS 的半关闭、超时及阻塞写关闭另外由 [reverse_tls_test.go](../internal/tunnel/reverse_tls_test.go) 覆盖。同账号双隧道测试同时开启反向两端 TLS，并覆盖 WS/WSS。

所有夹具均开启双向随机噪音，检查发送重放缓存、接收交付队列不超过配置上限。业务结束后等待 FIN/RESET/ACK 清空；退出时检查客户端/服务端会话、流、缓存、传输工作退出，以及中继排队字节、活动连接、协程和文件描述符回落。

## 可重复执行的命令

常规矩阵随普通 Go 测试执行，也可单独运行：

```sh
GOPROXY=off go test -mod=mod -race ./internal/tunnel \
  -run '^TestReverseWeakNetwork' -count=1 -timeout=6m -v
```

扩大负载的验证：

```sh
SGATE_REVERSE_STRESS=1 GOPROXY=off go test -mod=mod -race ./internal/tunnel \
  -run '^TestReverseWeakNetwork' -count=1 -timeout=8m -v
```

常规单流数据块为 128 KiB、长断流 2 轮、外部 RST 12 轮；扩大负载后分别为 512 KiB、6 轮、48 轮。每方向字节完整性、原目标连接保留、缓存界限及退出清理的断言保持一致。测试中的 900 ms 心跳阈值仅用于快速触发场景，没有修改生产默认心跳或会话期限。

同账号双隧道和错误隔离可分别重复执行：

```sh
GOPROXY=off go test -mod=mod -race ./internal/tunnel \
  -run '^TestReverseWeakNetwork(SameAccountTwoTunnels|HalfCloseENOTCONNIsolation)$' \
  -count=3 -timeout=2m -v
```

## 本次执行记录

在本机运行真实 TCP/WS/WSS 并启用 Go 竞态检测：

- 常规矩阵：14 个子场景通过，89.745 秒；各子场景均检查退出资源回落。
- 同账号双隧道：WS/WSS 各重复两次通过，6.383 秒。
- 扩大负载完整矩阵：16 个子场景通过，357.163 秒；4 种 WS/WSS/TLS 组合合计每方向精确校验 57 MiB 主负载，另含健康流、RST 与双隧道验证。16 次退出检查均恢复到本次测试基线（3 个协程、6 个文件描述符）。
- 扩大负载中，各组合分别执行 6 轮单向长断流和 48 轮外部 RST；目标连接与旧会话按预期保留。
- 原有反向 TLS、压力、背压、半关闭与 RESET/日志相关竞态回归重复两次通过（9.939 秒）；应用层和服务端入口完整竞态回归通过（70.356 / 1.684 秒）。
- 最终显式检查 `SetLinger(0)` 成功的真实 RST 回归通过（26.163 秒）；`go vet -mod=mod ./...` 与文档链接检查通过。

这些记录是本地有序网络模拟测试，不等同于 Linux 公网现场复现；`ENOTCONN` 的确切错误码使用显式注入验证，真实 RST 用例不要求各平台返回相同 errno。若现场仍有整条隧道反复断开，需要结合两端同会话 ID 的断开原因和断开前后的时间序列继续分析。
