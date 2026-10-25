#!/bin/sh
# 安装程序和部署文件，不启动服务，也不导入开发机上的管理员账号或会话。
set -eu

source_dir=$1
binary_dir=$2
prefix=${3%/}
destdir=${4%/}
case "$prefix" in
    /*) ;;
    *) printf '%s\n' 'PREFIX 必须是绝对路径，且不能为 /。' >&2; exit 1 ;;
esac
case "$destdir" in
    ''|/*) ;;
    *) printf '%s\n' 'DESTDIR 必须为空或绝对路径。' >&2; exit 1 ;;
esac
destination=$destdir$prefix

configs='sgate-client.cf sgate-server.cf'
scripts='sgate-client-start.sh sgate-client-stop.sh sgate-server-start.sh sgate-server-stop.sh'
sample_configs='ssh-client.cf ssh-server.cf vpn-client.cf vpn-server.cf web-client.cf'
sample_scripts='sshc-start.sh sshc-stop.sh sshd-start.sh sshd-stop.sh'
assets='index.html app.css app.js'

require_file() {
    if [ ! -f "$1" ]; then
        printf '缺少安装文件：%s；请检查源码或部署包是否完整。\n' "$1" >&2
        exit 1
    fi
}

# 先检查全部输入，避免缺失页面或程序时留下半套安装文件。
for name in sgate-client sgate-server; do
    if [ ! -f "$binary_dir/$name" ] || [ ! -x "$binary_dir/$name" ]; then
        printf '程序不存在或不可执行：%s\n请先以普通用户执行 make build，再执行 sudo make install。\n' "$binary_dir/$name" >&2
        exit 1
    fi
done
for name in $configs $scripts LICENSE; do require_file "$source_dir/$name"; done
for name in $sample_configs $sample_scripts README.md; do require_file "$source_dir/samples/$name"; done
for name in $assets; do require_file "$source_dir/web/admin/$name"; done
for name in gin-service go-service; do require_file "$source_dir/third_party/$name/LICENSE"; done
if [ ! -f "$source_dir/sgate.key" ] && ! [ -e "$destination/conf/sgate.key" ] && ! [ -L "$destination/conf/sgate.key" ]; then
    if [ ! -r /dev/urandom ]; then
        printf '%s\n' '无法读取随机源 /dev/urandom，请先准备 sgate.key。' >&2
        exit 1
    fi
fi

ensure_directory() {
    if [ ! -d "$destination/$1" ]; then
        install -d -m "$2" "$destination/$1"
    fi
}

ensure_directory . 0755
for name in sbin bin web/admin logs var share/licenses samples; do ensure_directory "$name" 0755; done
ensure_directory conf 0700

# 使用同一文件系统的临时目录再重命名，避免覆盖正在运行的 Linux 可执行文件。
stage=$(mktemp -d "$destination/.install.XXXXXX")
trap 'rm -rf "$stage"' EXIT HUP INT TERM

replace_file() {
    install -m "$3" "$1" "$stage/file"
    mv -f "$stage/file" "$2"
}

for name in sgate-client sgate-server; do replace_file "$binary_dir/$name" "$destination/sbin/$name" 0755; done
for name in $scripts; do replace_file "$source_dir/$name" "$destination/bin/$name" 0755; done
for name in $sample_scripts; do replace_file "$source_dir/samples/$name" "$destination/samples/$name" 0755; done
replace_file "$source_dir/samples/README.md" "$destination/samples/README.md" 0644
for name in $assets; do replace_file "$source_dir/web/admin/$name" "$destination/web/admin/$name" 0644; done
replace_file "$source_dir/LICENSE" "$destination/LICENSE" 0644
for name in gin-service go-service; do
    replace_file "$source_dir/third_party/$name/LICENSE" "$destination/share/licenses/$name.LICENSE" 0644
done

render_config() {
    # 仅重定位路径参数，保持地址、密码、证书和其它应用参数的原值。
    awk -v prefix="$prefix" '
        function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s); return s }
        function quoted(s) { gsub(/\\/, "\\\\", s); gsub(/"/, "\\\"", s); return "\"" s "\"" }
        function relocate(s,    base, p) {
            base = "/opt/soft/sgate/"
            p = index(s, base)
            if (p == 1) return prefix "/" substr(s, length(base) + 1)
            return s
        }
        /^[ \t]*#/ { print; next }
        {
            equal = index($0, "=")
            if (!equal) { print; next }
            key = trim(substr($0, 1, equal - 1))
            if (key !~ /^(master_command|master_home|master_log|master_stdout|master_stderr|key-file|admin_web_dir|admin_users_file|admin_session_dir|admin_setup_token_file)$/) {
                print; next
            }
            value = trim(substr($0, equal + 1))
            if (value ~ /^".*"$/) value = substr(value, 2, length(value) - 2)
            if (key == "key-file" && value == "sgate.key") value = prefix "/conf/sgate.key"
            else if (key == "admin_web_dir" && (value == "web/admin" || value == "../web/admin")) value = prefix "/web/admin"
            else if (key ~ /^admin_(users_file|session_dir|setup_token_file)$/ && value ~ /^(\.\.\/)?conf\//) {
                sub(/^\.\.\//, "", value)
                value = prefix "/" value
            }
            else value = relocate(value)
            # go-service 对 master_* 使用原始字符串，不会移除引号。
            if (key ~ /^master_/) print substr($0, 1, equal) " " value
            else print substr($0, 1, equal) " " quoted(value)
        }
    ' "$1" > "$stage/config"
}

# 专用示例不进入正式 conf/。两种配置都保留已有文件，以免覆盖手工修改的示例。
install_config() {
    render_config "$source_dir/$1"
    replace_file "$stage/config" "$destination/$2.dist" 0600
    if [ -e "$destination/$2" ] || [ -L "$destination/$2" ]; then
        printf '保留现有配置：%s\n' "$destination/$2"
    else
        replace_file "$stage/config" "$destination/$2" 0600
    fi
}

for name in $configs; do install_config "$name" "conf/$name"; done
for name in $sample_configs; do install_config "samples/$name" "samples/$name"; done

if [ ! -e "$destination/conf/sgate.key" ] && [ ! -L "$destination/conf/sgate.key" ]; then
    if [ -f "$source_dir/sgate.key" ]; then
        replace_file "$source_dir/sgate.key" "$destination/conf/sgate.key" 0600
    else
        # /dev/urandom 提供 32 字节随机密钥；先检查 od 成功，再转为程序接受的十六进制。
        od -An -v -N32 -tx1 /dev/urandom > "$stage/random"
        tr -d ' \n' < "$stage/random" > "$stage/key"
        [ "$(wc -c < "$stage/key" | tr -d ' ')" = 64 ]
        printf '\n' >> "$stage/key"
        replace_file "$stage/key" "$destination/conf/sgate.key" 0600
        printf '%s\n' '已生成 conf/sgate.key；部署两端时请使用同一份密钥。'
    fi
fi

printf '安装完成：%s\n配置：%s/conf（现有配置和密钥保留，新模板为 *.cf.dist）。\n' "$destination" "$prefix"
