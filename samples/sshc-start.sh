#!/bin/sh

set -eu
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# 仅控制 samples/ 中对应的专用示例，不读取正式部署配置。
config="$script_dir/ssh-client.cf"
exec "${MASTER_CTL:-/opt/soft/acl-master/bin/master_ctl}" -f "$config" -a start
