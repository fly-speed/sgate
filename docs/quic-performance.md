# QUIC 性能基线与验证

本项目保留三条可对照路径：WSS、`quic-mode=legacy`（v1 单流 WebSocket 封装）、`quic-mode=native`（v2 原生多流，默认）。原生模式同时改为逐业务流的序号、应用 ACK、接收交付队列和重放状态；不能只通过多开几个 QUIC 流获得相同语义。

## 复现基线

每次选择一个新的输出目录。脚本使用 `go.mod` 指定的依赖编译测试二进制，每个样本在一个全新的进程中执行，不修改部署配置，不要求额外第三方工具：

```sh
python3 scripts/benchmark-transports.py \
  --output artifacts/quic-loopback-run \
  --profiles loopback --modes wss,quic-v1,quic-v2 \
  --flows 1,8,32 --bytes 4194304 --repeats 3

python3 scripts/benchmark-transports.py \
  --output artifacts/quic-loss-run \
  --profiles udp-loss --modes quic-v1,quic-v2 \
  --flows 1,8,32 --bytes 262144 --repeats 3
```

目录中保留逐次 JSON、日志、汇总和 manifest。manifest 包含测试源码及二进制 SHA-256、平台和参数。失败样本也写入 JSON 并保留日志；全部用例运行完成后，只要有失败，脚本返回非零。空目录应由脚本新建，避免覆盖旧结果。

为避免互相干扰，性能采样时不要同时执行竞态测试、编译、压测或其他 CPU 密集任务。不同 Go 版本、CPU、内核、负载及缓存状态均可能改变结果。`artifacts/` 不纳入版本控制；2026-11-02 精简存档见 [基线数据](quic-baseline.json)；后续资源与弱网验证见 [优化交付记录](quic-optimization-status.md)。

## 测量口径

- **吞吐**：连接及业务流预热后，多路 TCP 同时向回显后端传输，逐字节核对结果；应用收发有效负载总量除以时间，单位 Mbps，两个方向合计。它不等同于单向下载速率，也不包含 TLS/认证或新建业务流首包耗时。
- **空闲延迟**：大块传输完成后，各业务流并发进行 50 次小请求回显，报告 nearest-rank 方法的样本内 P50/P95/P99；50 个观测的 P99 为最大值。
- **负载下延迟**：已有业务流持续进行经过核对的 64 KiB 传输，另开一条小请求流采样。最多 50 次，采样阶段限时约 5 秒，单次请求超时 3 秒；只有完成全部 50 次才输出该阶段 P95/P99。未完成和超时单独记录，汇总中的空值不代表零延迟。
- **CPU**：大块传输阶段的进程 user+system CPU 时间，包含客户端、服务端、回显后端和负载生成器，可能大于墙钟时间。不是某一端的 CPU 使用率。
- **内存**：Go `TotalAlloc` 的阶段增量是累计分配量；RSS 是从进程启动到大块传输结束的峰值，包含设置阶段和生成器。两者均不能直接当作某一端的常驻内存。CPU/RSS 目前在 macOS/Linux 采集，其他平台标记为不可用。
- **流量**：原有 `tx_bytes/rx_bytes` 为传输流字节。`quic.packet_tx_bytes/packet_rx_bytes` 包括 QUIC 包头、加密、ACK 和重传，不包括 UDP/IP 头。WSS 流字节与 QUIC 包字节不是同一层，不能直接比较为线上带宽节省比例。
- **汇总**：每组重复样本取中位数，同时保留样本数、失败数、混合负载超时及采样未完成数。失败样本不进入成功样本中位数，也不会从报告中消失。

## UDP 弱网配置的边界

`udp-loss` 使用真实 UDP 数据报中继，在两个方向各增加 10 ms 延迟，并确定性丢弃每第 97 个数据报。每方向队列限制为 256 个数据报；队列满也会丢包，因此实际丢包量不一定等于 1/97。当前原始报告中的 relay 丢包数覆盖整个样本，QUIC 包指标的增量覆盖大块传输阶段，二者统计时段也不同。

这个中继只用于比较 QUIC v1/v2，WSS 在该配置下自动跳过。它不模拟 TCP 的真实 IP 丢包或 ECN。新增 `udp-matrix` 可组合延迟、丢包、字节限速、乱序和 UDP 负载大小限制，但这些仍是本机数据报中继，不能等同真实物理链路。不能把中继结果当作公网结论。需要 WSS/QUIC 的同条件真实弱网对照时，应在独立 Linux 环境用网络命名空间和 `tc netem` 对 TCP/UDP 同时施加相同延迟、丢包及限速，再重复采样；本轮未执行该环境的测试。

## 历史实测结果（2026-11-02）

环境为 macOS/arm64，Go 版本、平台与源码快照见数据 manifest；每组 3 次，以下为组内中位数。所有正式样本的有效负载核对均通过。

本机回环，32 条大流并发，额外一条小请求流：

| 模式 | 收发合计吞吐 Mbps | 负载下小请求 P99 ms | 大块阶段 CPU 秒 | 累计分配 MiB | 进程峰值 RSS MiB |
| --- | ---: | ---: | ---: | ---: | ---: |
| wss | 5492.7 | 8.17 | 1.96 | 2582.9 | 302.6 |
| quic-v1 | 1829.7 | 23.56 | 7.01 | 2748.2 | 299.9 |
| quic-v2 | 1772.9 | 3.27 | 6.74 | 2109.6 | 304.8 |

本轮原生多流相对旧 QUIC 的负载下 P99 约降低 86.1%，吞吐约下降 3.1%，累计分配约减少 23.2%。CPU 时间及峰值 RSS 的变化见表；WSS 在本机的大块吞吐仍较高。每组只有 3 次重复，每次混合负载最多 50 个小请求，不足以证明总体 P99 或生产 SLA。单路/8 路结果也保存在数据文件中，不能由这张表推断所有网络上的结果。

UDP 中继（双向各 10 ms，丢弃每第 97 个数据报，队列满时也丢包）：

| 模式 | 业务流数 | 吞吐 Mbps | 空闲小请求 P99 ms | 完成 50 次负载下采样的重复次数 |
| --- | ---: | ---: | ---: | ---: |
| quic-v1 | 1 | 24.6 | 72.10 | 3/3 |
| quic-v1 | 8 | 16.1 | 46.93 | 0/3 |
| quic-v1 | 32 | 13.5 | 46.72 | 0/3 |
| quic-v2 | 1 | 28.3 | 71.54 | 3/3 |
| quic-v2 | 8 | 15.1 | 43.69 | 3/3 |
| quic-v2 | 32 | 13.0 | 44.50 | 0/3 |

采样未完成的组不提供混合负载 P99，也不据此声称原生模式已解决所有弱网延迟问题。吞吐与完整采样次数应一起看；改造的收益取决于工作负载。独立的慢后端隔离与断线去重由正确性测试验证。

探索阶段还有旧 QUIC 混合负载达到两分钟应用 deadline 的失败日志，保留在 `artifacts/quic-native-comparison/` 和 `artifacts/quic-final-loss/`，未混入正式有界采样的成功中位数。初次采样和后续有界采样规则不同，不合并比较。对“失败”和“采样未完成”分别计数，避免将测量期限当成传输层故障证据。

## 缓存与部署参数

客户端、服务端均支持以下独立配置，大小支持 K/M，`0` 表示按 `buffer-bytes` 派生（进程预算例外）：

| 参数 | 默认或含义 |
| --- | --- |
| `quic-stream-window` | 每条 QUIC 流的最大接收窗口；默认 `buffer-bytes`，至少 16 KiB |
| `quic-connection-window` | 每条 QUIC 连接的最大接收窗口；默认 `buffer-bytes`，至少 16 KiB |
| `quic-stream-buffer` | 每业务流、每方向的应用队列上限；默认 max(完整帧大小, buffer-bytes/8) |
| `quic-replay-bytes` | 原生会话全部业务流的重放队列预算；默认 `buffer-bytes` |
| `quic-receive-bytes` | 原生会话全部业务流的接收队列预算；默认 `buffer-bytes` |
| `memory-budget-bytes` | 进程共享的受管缓冲区预留上限；默认 0，不启用限制 |
| `quic-initial-packet-bytes` | 初始 UDP 负载大小，默认 1200，范围 1200～1452；保留 PMTU 发现 |

应用单流队列必须同时放得进重放和接收预算，并至少容纳一个完整业务帧。两端可以配置不同的接收额度；原生握手通过认证字段协商单流额度，并兼容未提供新字段的 v2 对端。窗口大小仍应结合带宽时延积测量。

例如显式启用 `memory-budget-bytes = "256M"`。该预算在分配会话队列、建立 QUIC 连接之前预留，覆盖所有映射、离线保留会话、每连接 QUIC 接收窗口和按最大流数预留的工作帧容量；不足时拒绝新准入，关闭后释放。它不是 RSS 上限，不含 Go runtime、TLS/QUIC 元数据、重传跟踪元数据或内核缓冲区。默认不限制是为保留现有部署容量，生产值需根据会话数与实际 RSS 调整。

修改 QUIC 流/连接窗口、初始包大小、启用 QUIC 时的最大流数及进程总预算需要重启整个进程；应用队列参数通过代理重启生效。管理界面会校验范围并拒绝不能在线替换的参数变化。

## 运行指标

管理 API 的 `stats` 响应（服务端例如 `/admin/api/stats`）新增 `quic` 对象：

| 字段 | 含义 |
| --- | --- |
| `connections_total` / `active_connections` | QUIC 连接累计/当前数量，包含握手中的连接 |
| `packets_tx` / `packets_rx` | QUIC 包发送/接收计数 |
| `packet_tx_bytes` / `packet_rx_bytes` | QUIC 包字节，含重传，不含 UDP/IP 头 |
| `lost_packets` | QUIC 判定丢失的包数，不是精确物理链路丢包率 |
| `max_smoothed_rtt_ms` | 活跃连接中最大的平滑 RTT；无活跃连接时为 0 |
| `congestion_window_bytes` / `bytes_in_flight` | 活跃连接的拥塞窗口/在途字节之和 |
| `native_streams_total` | 累计建立的原生业务 QUIC 通道数，恢复时重新建立通道会再次计数 |
| `application_replay_bytes` | 连接恢复时重新发送的已发送业务负载，不等于 QUIC 的网络重传 |

计数字段累计到进程退出；活跃连接数、RTT 和窗口是当前快照。原生握手、控制及业务流字节继续进入总量及按 IP 的统计。页面分别显示活跃连接、RTT、丢包判定、包字节和应用重放，避免将不同层次的数据混用。

## 正确性验证

```sh
go test -mod=mod -race ./internal/tunnel ./internal/app \
  -run '^(TestNativeQUIC|TestQUIC)' -count=3

go test -mod=mod -race ./... -timeout 20m
```

专项覆盖：认证与证书主机名校验、旧/新协议恢复隔离、多路双向数据、慢后端隔离、带未确认数据的恢复及去重、反向代理、半关闭、持续开关流后的元数据回收、控制流与噪音背压、真实 UDP 丢包。WS/WSS 及旧 QUIC 测试仍保留。基准中的性能差异不作为 CI 的硬阈值；传输完整性、资源回收与协议边界由测试验证。

## 新增观测与健康检查（2026-11-03）

认证后的 `stats` 另含 `memory`（limit/reserved/peak/rejected）和 `health`；管理页面显示预算使用与 QUIC 监听降级。认证后的 `/admin/api/health`（自定义管理前缀时相应调整）健康返回 200，降级返回 503；未认证返回 401。UDP 异常不会被仍正常工作的 TCP 监听掩盖。

监听意外退出时，后台关闭故障套接字，以 100 ms 到 5 s 的退避重新绑定同一地址，记录 failures、recoveries、last_error；进程停止会取消重试。可发送时先通知对端关闭 QUIC 连接，促使立即恢复。保留应用会话和业务 TCP 套接字，但仍受会话 TTL、恢复超时及缓冲容量约束；真正的网络断连需等传输/心跳检测。初次绑定失败仍作为启动错误返回。

`quic` 新增指标：

| 字段 | 口径 |
| --- | --- |
| `retransmitted_stream_bytes` / `stream_retransmit_packets` | 已发送 STREAM 偏移的重叠负载字节 / 含重发 STREAM 或 FIN 的包；含控制流，不含 CRYPTO、ACK、包头 |
| `retransmission_tracking_evictions` | 每连接最多跟踪 2048 条流，LRU 淘汰次数；淘汰后很晚到来的重传可能漏计 |
| `connection_blocked_frames` / `stream_blocked_frames` / `stream_count_blocked_frames` | 发出的 DATA_BLOCKED、STREAM_DATA_BLOCKED、STREAMS_BLOCKED 帧数量，非阻塞时长 |
| `application_blocked_events` / `application_blocked_ms` | 应用队列容量不足的等待次数与累计时间；并发等待累加，不是墙钟时间 |
| `pto_events` | PTO 计数更新为正数的事件数 |
| `min_active_mtu` | 活跃连接中已知最小 QUIC UDP 负载 MTU；未知时 0 |
| `close_reasons` | 有界分类：idle_timeout、handshake_timeout、remote_application_close、local_application_close、transport_error、canceled、other |

这些指标与 `application_replay_bytes` 分开统计；不能将 STREAM 重传字节解释为完整线上重传字节，也不能把 transport_error 当作应用认证失败原因。

## 参数矩阵与 Linux 同条件脚本

本机 UDP 矩阵（WSS 自动跳过，保留 skipped.json）：

```sh
python3 scripts/benchmark-transports.py --output artifacts/quic-matrix \
  --profiles udp-matrix --modes quic-v1,quic-v2 --flows 1,4 \
  --delays-ms 0,5 --loss-every 0,97 --rates-kbps 0,4096 \
  --mtu 1250 --reorder-every 11 --bytes 65536 --repeats 3
```

Linux 同条件 TCP/UDP 测试命令如下。**按用户确认，仅交付脚本，未进行 Linux 运行实测**。需要 Linux、root（具备网络命名空间权限）、Go、iproute2 的 ip/tc、ethtool 及相应内核功能。每个样本创建独立网络命名空间，核验命名空间身份后设置其中的 lo/qdisc；仅隧道端口进 netem，避免重复整形业务回显端口。关闭分段/聚合卸载，结束清理命名空间，不操作宿主机 qdisc。设置/执行/清理失败记为失败并保留日志。硬中断或进程被 SIGKILL 后，可能需要手动清理 `sgate-bench-*` 命名空间。

```sh
sudo python3 scripts/benchmark-transports.py --output artifacts/quic-linux-netem \
  --profiles linux-netem --modes wss,quic-v1,quic-v2 --flows 1,8,32 \
  --delays-ms 5,20 --loss-every 0,97 --rates-kbps 4096 \
  --mtu 1250 --reorder-every 11 --bytes 262144 --repeats 3
```

Linux netem 的丢包/乱序是概率配置，UDP 中继是每第 N 包触发，二者不是逐包相同序列；`--delays-ms` 为每方向延迟，`--rates-kbps` 为整形队列速率，0 表示不限制，`--mtu` 为 UDP 负载上限（Linux 对应 IPv4 MTU=该值+28）。Linux 重排要求正延迟。命名空间里的回环测试也不能替代跨公网、真实 NAT 和设备测试。

## QUIC 白噪音按每秒速率发送

QUIC 原生 v2 和 legacy v1 均使用小帧限速；WS/WSS 保持原有批次语义。**这是发送端语义变化，更新两端程序并重启后生效**，不需要升级线上的噪音帧格式。每端独立控制发送；新旧端混用时旧端仍按批发送。

| 参数 | WS/WSS | QUIC |
| --- | --- | --- |
| `noise-min-bytes` / `noise-max-bytes` | 每批随机负载字节 | 每条隧道随机目标负载字节/秒 |
| `noise-interval` 或随机间隔上下限 | 每批完成后的等待 | 目标速率重选周期，至少保持 1 秒 |
| 间隔全部为 0 | 关闭噪音 | 关闭噪音 |
| `noise-traffic` / `noise-idle` / `noise-empty` | 对应场景启停 | 对应场景启停，恢复不补发 |

例如 QUIC 中 min=max=`1M` 表示每条隧道每方向约 1 MiB/s 的噪音负载目标，不再是每 100 ms 发 1 MiB。min=`1M`、max=`10M` 仍会随机变化，想稳定应缩窄范围或使用相同上下限。多条隧道会叠加；它不是进程全局或双向合计限额。

调度器每次只产生一个到期小帧（最多 16 KiB，通常按约 20 ms 的发送量拆分）；低速时允许更小的负载。成功写入后再计算下次时间。暂停、长时间写阻塞、重连均不会累计可补发额度；场景从忙碌转为空闲时会重新检查并逐步恢复，无需等待下次心跳。大小、间隔和策略热更新会重新建立发送计划，无关的重连参数修改不会推迟噪音。

这是负载速率目标，不是线上硬带宽上限或精确交付承诺：线程调度、写入耗时及拥塞会降低实际速率，采样边界可能包含一个小帧的误差；原生 JSON/base64、加密、QUIC/UDP/IP、ACK、底层网络重传另有开销。噪音由 `crypto/rand` 生成，接收后丢弃，不进入业务 TCP 或应用重放缓存；底层可靠 TCP/QUIC 仍可能重传。

前端同时展示两种单位，配置校验提示与当前运行状态中的 `quic_unit=bytes_per_second` 明确 QUIC 口径。总流量图仍包括协议与业务流量，噪音负载曲线更适合对照配置值。

验证包括 `TestQUICNoisePacerBudgetAndNoCatchup`（低速至 10 MiB/s 的逐秒预算、暂停及拥塞不补发）、`TestQUICNoisePacerLiveChanges`、`TestQUICNoisePayloadRate`（真实 native/legacy 双端 100 ms 窗口速率约束）、`TestTransportNoisePolicyParity`（双向独立开关、固定/随机重选、忙闲切换和在线更新）、`TestNativeQUICNoiseRandomPayloadOnWire`（两个方向解密检查小帧随机负载）。WS/WSS 批次与调度测试保留。

本次验证结果与实测样本见 [QUIC 每秒速率验证](quic-noise-rate-validation.json)。
