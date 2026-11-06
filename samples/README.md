# 配置示例

[English](#configuration-examples)

部署时从项目根目录的 `sgate-client.cf` 和 `sgate-server.cf` 开始。客户端可通过 `proxies` 或 `proxies-file` 配置多条映射。由客户端显式指定的后端须加入服务端的 `allowed-targets`。

| 示例 | 用途 |
| --- | --- |
| `ssh-client.cf`、`ssh-server.cf` | 转发 SSH。配套 `sshc-*.sh` 和 `sshd-*.sh` 只操作对应示例，可用 `MASTER_CTL` 指定管理命令 |
| `web-client.cf` | 转发内网 Web 服务 |
| `vpn-client.cf`、`vpn-server.cf` | 通过 TCP 代理转发 OpenVPN |
| `sgate-vpn-client.conf`、`sgate-vpn-server.conf` | 供独立 `sgate-vpn` 命令读取的 IPv4 VPN 配置。集成 VPN 在两端的主 `.cf` 中设置 `vpn-*` 参数 |
| `windows-client.cf` | Windows 客户端打包模板 |
| `sgate-proxies.json` | 通过客户端 `proxies-file` 加载多条映射，每条映射可使用单独的凭据 |
| `sgate-accounts.json` | 通过服务端 `accounts-file` 加载代理账号。客户端用 `username` 和 `password` 登录 |
| `sgate-reverse-accounts.json` | 为同一代理账号配置多条反向 TCP 映射。`listen` 在服务端绑定，`target` 由客户端连接 |

部署前修改示例中的地址、密码、密钥和证书路径。JSON 账号示例默认禁用，修改凭据后再启用所需条目。包含密码的 JSON 文件应限制访问权限，例如在 Unix 上设置为 `0600`。反向代理还需客户端对每条映射显式授权（JSON 中设为 `"allow_reverse": true`）。旧式单映射配置使用 `allow-reverse = true`。

Unix 安装将所选角色的示例放在 `${PREFIX}/samples/`，主配置放在 `${PREFIX}/conf/`。重复安装保留已有配置，将更新过的模板另存为 `.dist` 文件。若从旧的专用服务迁移，先停止对应服务，再将监听和后端合并到主配置。测试证书和公开测试私钥仅用于本机验证。 [测试证书说明](test-tls/README.md) · [VPN 指南](../docs/vpn.md)

## Configuration examples

Start with the root-level `sgate-client.cf` and `sgate-server.cf`. The client can define multiple mappings through `proxies` or `proxies-file`. Add any backend explicitly selected by the client to the server's `allowed-targets`.

| Example | Use |
| --- | --- |
| `ssh-client.cf`, `ssh-server.cf` | Forward SSH. The accompanying `sshc-*.sh` and `sshd-*.sh` scripts operate on these examples and accept the `MASTER_CTL` environment variable |
| `web-client.cf` | Forward an internal web service |
| `vpn-client.cf`, `vpn-server.cf` | Forward OpenVPN over TCP |
| `sgate-vpn-client.conf`, `sgate-vpn-server.conf` | Configure the standalone `sgate-vpn` command. For integrated VPN, set the `vpn-*` options in the main `.cf` files |
| `windows-client.cf` | Windows client packaging template |
| `sgate-proxies.json` | Load multiple client mappings through `proxies-file`, with optional per-mapping credentials |
| `sgate-accounts.json` | Load server tunnel accounts through `accounts-file`. Clients authenticate with `username` and `password` |
| `sgate-reverse-accounts.json` | Configure multiple reverse TCP mappings for one account. `listen` binds on the server and the client connects to `target` |

Update the example addresses, passwords, key paths, and certificate paths before deployment. The JSON account examples ship disabled. Change the credentials before enabling an account. Restrict access to JSON files containing passwords, for example with mode `0600` on Unix. For reverse forwarding, grant consent on each intended client mapping with `"allow_reverse": true`. Legacy single-mapping configurations use `allow-reverse = true`.

On Unix, installation places examples for the selected roles in `${PREFIX}/samples/` and the main configuration in `${PREFIX}/conf/`. Reinstalling preserves existing configuration and writes updated templates to `.dist` files. To migrate from older dedicated services, stop them before merging their listeners and targets into the main configuration. Use the bundled test certificate and public test private key for local verification only. [Test TLS instructions](test-tls/README.md) · [VPN guide (Chinese)](../docs/vpn.md)
