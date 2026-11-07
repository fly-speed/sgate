#!/bin/sh
# Start a foreground WS test server using command-line options only.
# Additional arguments override the defaults below; Ctrl+C stops the server.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ -x "$script_dir/../sbin/sgate-server" ]; then
    binary="$script_dir/../sbin/sgate-server"
    key_file="$script_dir/../conf/sgate.key"
else
    binary="$script_dir/bin/sgate-server"
    key_file="$script_dir/sgate.key"
fi

if [ ! -x "$binary" ]; then
    printf '%s\n' 'sgate-server not found; run make server in the source directory first.' >&2
    exit 1
fi

exec "$binary" -alone \
    -master_service '127.0.0.1|8081' \
    -target 127.0.0.1:8080 \
    -allowed-targets 127.0.0.1:8080 \
    -key-file "${SGATE_KEY_FILE:-$key_file}" \
    -password "${SGATE_PASSWORD:-sgate-test-password}" \
    "$@"
