#!/bin/sh

set -eu
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# 安装后读取相邻 conf/；源码目录直接运行时读取当前目录的配置。
config="$script_dir/../conf/sgate-client.cf"
[ -f "$config" ] || config="$script_dir/sgate-client.cf"
exec "${MASTER_CTL:-/opt/soft/acl-master/bin/master_ctl}" -f "$config" -a start
