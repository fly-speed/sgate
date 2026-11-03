# 安装测试证书

证书与私钥随客户端和服务端安装到 `${PREFIX}/conf/`（默认 `/opt/soft/sgate/conf/`），用于本机 QUIC/WSS 安装测试，不会自动启用。本说明安装到 `${PREFIX}/samples/test-tls/README.md`。

这是随包公开分发的自签名证书和测试私钥，仅用于测试，不能用于生产。证书有效期至 2027-11-03，SAN 包含 `localhost`、`127.0.0.1`、`::1`。其他 IP/域名或证书过期时请生成自己的证书。不要把该证书加入操作系统的全局信任列表。

服务端配置（默认安装路径）：

```ini
quic-listen = 127.0.0.1:1080
tls-cert = /opt/soft/sgate/conf/tls-cert.pem
tls-key = /opt/soft/sgate/conf/tls-key.pem
```

客户端配置：

```ini
server = quic://127.0.0.1:1080/tunnel
tls-ca = /opt/soft/sgate/conf/tls-cert.pem
```

其余密钥、认证及后端配置照常填写，客户端和服务端使用相同的 sgate.key；已有多映射配置需修改相应映射的 server。更改监听地址及证书后重启服务端进程。自定义 PREFIX 时替换上述路径。

安装目录权限为 0700，私钥为 0600，确保运行服务的账号可读取；重复安装保留已有证书/私钥，避免拆散自定义证书对。
