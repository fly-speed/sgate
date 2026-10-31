#!/bin/sh

set -eu
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# Preserve the captured service configuration during a Web-triggered restart.
config=${SGATE_CONFIG:-}
if [ -z "$config" ]; then
    config="$script_dir/../conf/sgate-client.cf"
    [ -f "$config" ] || config="$script_dir/sgate-client.cf"
fi
# master_ctl -t is in milliseconds; 30000 gives startup 30 seconds.
set -- -f "$config" -a stop -t "${MASTER_CTL_TIMEOUT:-30000}"
if [ -n "${MASTER_SOCKET:-}" ]; then
    set -- "$@" -s "$MASTER_SOCKET"
fi
exec "${MASTER_CTL:-/opt/soft/acl-master/bin/master_ctl}" "$@"
