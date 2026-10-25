# 专用配置示例

正式部署使用项目根目录的 `sgate-client.cf` 和 `sgate-server.cf`。客户端通过 `proxies` 配置多个本地监听和远程后端；服务端通过目标允许列表及 `/tunnel?addr=host:port` 服务多个后端，不必按 SSH、VPN、Web 各启动一套进程。

本目录保留原有 SSH、VPN、Web 专用配置和四份 SSH 启停脚本，便于参考或单独部署。业务地址、密码、证书和其它应用参数保持原值；管理页面和账号、会话路径已按新的目录层级调整。外部密钥、证书及 `acl_master` 需自行准备，示例不会自动注册或启动。

- `ssh-client.cf`、`ssh-server.cf`：SSH 专用示例。
- `vpn-client.cf`、`vpn-server.cf`：TCP OpenVPN 专用示例。
- `web-client.cf`：Web 代理客户端示例。
- `sshc-start.sh`、`sshc-stop.sh`、`sshd-start.sh`、`sshd-stop.sh`：只操作本目录对应的 SSH 配置，支持 `MASTER_CTL` 环境变量。

从源码目录参考配置运行，例如 `./bin/sgate-client -alone -f samples/ssh-client.cf`。首次使用应核对地址、认证密码、密钥和证书，按部署环境修改配置。

`make install` 把本目录安装到 `${PREFIX}/samples/`，正式配置仅放在 `${PREFIX}/conf/sgate-client.cf`、`${PREFIX}/conf/sgate-server.cf`；默认 `${PREFIX}` 为 `/opt/soft/sgate`。重复安装保留已存在的示例配置，更新模板另存为 `.cf.dist`。

旧版本已安装在 `conf/` 的 SSH、VPN、Web 配置及旧启停脚本会保留，不会由安装程序自动删除。迁移前请先停止对应的旧服务，把需要的监听和后端映射合并到正式配置，再启用统一服务；确认后自行归档旧部署文件。
