#!/bin/sh
# Build a relocatable sgate release archive for macOS or Ubuntu.
set -eu

usage() {
    printf '%s\n' 'Usage: package/package-release.sh macos|ubuntu arm64|amd64 [output-dir]' >&2
    exit 2
}

[ "$#" -ge 2 ] && [ "$#" -le 3 ] || usage
platform=$1
arch=$2
case "$platform" in
    macos) goos=darwin ;;
    ubuntu) goos=linux ;;
    *) usage ;;
esac
case "$arch" in
    arm64|amd64) ;;
    *) usage ;;
esac

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
output=${3:-"$root/dist"}
mkdir -p "$output"
output=$(CDPATH= cd -- "$output" && pwd)
version=$(sed -n 's/^const Version = "\([0-9][0-9.]*\)"$/\1/p' "$root/internal/buildinfo/version.go")
[ -n "$version" ] || { printf '%s\n' 'Cannot determine the sgate release version.' >&2; exit 1; }
name="sgate-$version-$platform-$arch"

# Never package local deployment settings or the shared key from the checkout.
if ! git -C "$root" diff HEAD --quiet -- sgate-client.cf sgate-server.cf sgate-proxies.json samples; then
    printf '%s\n' 'Commit changes to the release templates or restore the tracked templates before packaging.' >&2
    exit 1
fi

stage=$(mktemp -d "$output/.sgate-package.XXXXXX")
trap 'rm -rf "$stage"' EXIT
trap 'exit 1' HUP INT TERM
package=$stage/$name
mkdir -p "$package/bin" "$package/scripts" "$package/docs" "$package/web/admin" \
    "$package/third_party/go-service" "$package/third_party/gin-service" "$package/third_party/licenses" "$package/samples/test-tls"

GOOS=$goos GOARCH=$arch CGO_ENABLED=0 make -C "$root" client server \
    BINDIR="$package/bin" MOD="${MOD:-readonly}"

for file in LICENSE sgate-client.cf sgate-server.cf sgate-proxies.json \
    sgate-client-start.sh sgate-client-stop.sh sgate-server-start.sh sgate-server-stop.sh \
    client-alone.sh server-alone.sh \
    scripts/install.sh scripts/test-vpn-linux.py docs/vpn.md docs/vpn-udp.md \
    web/admin/index.html web/admin/app.css web/admin/i18n.js web/admin/app.js \
    third_party/go-service/LICENSE third_party/gin-service/LICENSE \
    third_party/licenses/pion-dtls.LICENSE third_party/licenses/pion-logging.LICENSE third_party/licenses/pion-transport.LICENSE \
    samples/README.md samples/ssh-client.cf samples/ssh-server.cf samples/vpn-client.cf samples/vpn-server.cf \
    samples/web-client.cf samples/sgate-proxies.json samples/sgate-accounts.json samples/sgate-reverse-accounts.json \
    samples/sshc-start.sh samples/sshc-stop.sh samples/sshd-start.sh samples/sshd-stop.sh \
    samples/test-tls/README.md samples/test-tls/tls-cert.pem samples/test-tls/tls-key.pem; do
    cp "$root/$file" "$package/$file"
done

cat > "$package/install.sh" <<'INSTALL'
#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec sh "$root/scripts/install.sh" "$root" "$root/bin" "${PREFIX:-/opt/soft/sgate}" "${DESTDIR:-}" "${1:-all}"
INSTALL
chmod 0755 "$package/install.sh"

cat > "$package/README.txt" <<README
sgate $version for $platform ($arch)

Install both programs and the Web management files with sudo ./install.sh.
Use ./install.sh client or ./install.sh server to install just one role.
Set PREFIX to override /opt/soft/sgate. Set DESTDIR for an offline staging install.
The installer preserves existing configuration and keys, and never starts or restarts services.
For a fresh installation, review conf/ before starting either service.
The bundled certificate and private key are for testing.
For foreground command-line tests after installation, run bin/server-alone.sh and bin/client-alone.sh in separate terminals.
README

COPYFILE_DISABLE=1 tar -czf "$stage/$name.tar.gz" -C "$stage" "$name"
(
    cd "$stage"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$name.tar.gz"
    else
        shasum -a 256 "$name.tar.gz"
    fi
) > "$stage/$name.tar.gz.sha256"
mv -f "$stage/$name.tar.gz" "$stage/$name.tar.gz.sha256" "$output/"
printf '%s\n' "$output/$name.tar.gz"
