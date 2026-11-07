# sgate deployment and testing reference

[English quick start](../README.en.md) · [中文参考](reference.md)

A Go TCP tunnel with two executables, supporting WS/WSS over TCP and QUIC over UDP. `sgate-client` accepts local TCP connections and multiplexes them over one persistent, authenticated, AES-256-GCM encrypted WebSocket connection. `sgate-server` opens one TCP connection per stream to a fixed backend. Traffic flows in both directions.

This reference covers deployment, protocol behavior, and historical test results. Run source commands from the repository root and interpret test results in the context of their recorded platform and test conditions. The [architecture and tunnel protocol document (Chinese)](../docs/architecture.md) covers frame classes, command IDs, forward/reverse flows, encrypted JSON probes, ACKs, flow control, and replay.


For integrated VPN setup, including Windows clients, see the [English quick start](../README.en.md#enable-the-ipv4-vpn). The standalone `sgate-vpn` command is built with `make vpn` and installed with `make install-vpn`. For VPN routing, credentials, and platform validation, see the [VPN guide (Chinese)](vpn.md).


## UDP / QUIC tunnels

Set `quic-listen = "0.0.0.0:8443"` on the server together with `tls-cert` and `tls-key`. Set `server = "quic://gateway.example.com:8443/tunnel"` on the client; use `tls-ca` for a private CA or self-signed certificate. Keep the existing key, password/account and backend configuration. Both peers must support QUIC. The TCP tunnel remains available, and TCP and UDP may use the same port number. Adding `tls-cert`/`tls-key` to a previously plaintext deployment also enables HTTPS/WSS on that TCP endpoint; update existing TCP clients to `wss://`.

Open the UDP port in the firewall. QUIC requires TLS 1.3 and certificate verification against the URL hostname/IP. The URL requires an explicit port (1–65535). This is a private sgate QUIC protocol, not HTTP/3, and cannot pass through a regular TCP HTTP proxy or an HTTP/WebSocket-only reverse proxy. Management stays on the existing TCP HTTP/HTTPS endpoint. `quic-listen` defaults to empty (disabled); restart the entire process after changing it. In managed mode the single worker owns the UDP socket; do not list it as a TCP socket in acl_master.

Multi-port mappings may mix `quic://`, `ws://` and `wss://` servers. Backend selection, authentication, reverse proxy consent, noise, heartbeat and session resumption retain their existing behavior. The application-facing listeners and targets remain TCP; this does not add UDP application forwarding or automatic TCP fallback.

The default client setting is `quic-mode = "native"` (`sgate-quic-v2`): an authenticated control stream plus an independent bidirectional QUIC stream for each business connection. Sequence numbers, application ACKs, delivery queues and replay state are maintained per flow, with bounded session-wide memory and flow counts. Application AES-GCM encryption, forward/reverse proxying and half-closes remain supported.

Use `quic-mode = "legacy"` with an older server to retain `sgate-quic-v1` (WebSocket framing over one QUIC stream). New servers accept both ALPNs on the same UDP port. There is no automatic downgrade; changing mode requires restarting the proxy, and retained sessions cannot switch sequence semantics. This client-wide option applies to all QUIC mappings; WS/WSS mappings are unaffected.

The statistics API adds a `quic` object with packet bytes/counts, packets declared lost, active connection RTT/window/in-flight estimates, native flow count and application replay bytes. Existing totals count stream bytes; QUIC packet counters include encryption, headers, ACKs and retransmissions but exclude UDP/IP headers. Network RTT, application latency, application replay and network retransmission are distinct measurements. See [performance and validation](../docs/quic-performance.md) for methodology and reproducible commands. Streams still share congestion control and connection resources; higher throughput is not guaranteed.


Independent QUIC windows and queue budgets, shared process buffer admission, listener recovery, authenticated health checks and transport metrics are available. See [delivery status](../docs/quic-optimization-status.md) and [performance documentation](../docs/quic-performance.md) for defaults, restart requirements and evidence. `memory-budget-bytes` defaults to 0 (unlimited) and bounds managed reservations, not RSS. The Linux TCP/UDP netem script is delivered but has not been run on Linux.

Installations include a self-signed localhost certificate and a public test private key installed as `${PREFIX}/conf/tls-cert.pem` and `${PREFIX}/conf/tls-key.pem`. They are not enabled automatically and are for installation tests only; see [test TLS instructions](../samples/test-tls/README.md).

The live bandwidth chart shows separate TCP sent/received and QUIC sent/received series on both endpoints, with individual transport and IP views. Statistics expose cumulative `transports.tcp` and `transports.quic` byte counters globally and per IP. TCP includes WS/WSS and shared-port server management; QUIC uses stream bytes, excluding packet overhead and network retransmissions. Noise/business payload views remain aggregated across transports.

## acl_master integration

Only `sgate-client.cf` and `sgate-server.cf` remain at the project root for production use. Multiple client mappings use `proxies`; the server selects allowed backends through `/tunnel?addr=host:port`. See [samples/](../samples/README.md) for the previous SSH, VPN and Web examples.

The client uses go-service's TCP descriptor APIs. Both standalone and managed server startup now follow gin-service's `Init -> register routes -> Run` lifecycle, with no direct ListenAndServe branch. The local extension in [third_party/gin-service](../third_party/gin-service/README.md) is selected by go.mod replace; scripts/sync-go-vendor.py synchronizes standard vendor packages without altering the supplied reference repositories.

The framework owns listeners, TLS/HTTP serving, status monitoring, physical connection counts and callbacks, and joined shutdown. Hijacked connections stay counted until actually closed. With management enabled, the server keeps its HTTP Worker running and replaces only the proxy instance and its sessions; management requests, logins and counters remain available. The per-service context lifecycle replaces upstream global Wait/Stop to support cancellation, WSS cleanup and proxy reload. Gin remains absent from client dependencies. Use `make framework-test` for the local framework tests and `make vendor-sync` after modifying framework sources.

On Linux, run `python3 scripts/test-linux-integration.py --output artifacts/linux-integration` with native Go/C compilers, Python, OpenSSL, ps and acl_master/master_ctl installed. Override framework paths with `--master` and `--ctl`. It builds plain and race binaries, checks 16 deployment cases, and runs WSS stress in standalone and managed modes with random padding, 128 streams, transport faults and six process restart rounds per mode. Tests use isolated configurations and ports; choose an empty output directory. `verification.json` records the native platform, commands and results; cross-compilation is not accepted as a Linux runtime test.

Like the template examples, both programs support explicit standalone startup:

```sh
./bin/sgate-client -alone -f sgate-client.cf
./bin/sgate-server -alone -f sgate-server.cf
```

The server entry point calls `gin-service.Init`, `setRoute`, and `Run` directly. All WebSocket, management page, authentication, configuration, statistics and restart registrations are visible in `cmd/sgate-server/routes.go`. Application resource management stays in `internal/app/server.go`; a proxy path change uses the current proxy through `NoRoute` without mutating Gin routes while requests are running.

Existing direct commands without `-alone` remain valid. Without Web management, `-config` is optional and all application options may be passed on the command line. Enable embedded management with `-admin-enabled`. Under acl_master, leave `master_args` empty or include only CLI overrides, and omit `-alone`: the framework supplies `-f <service.cf>`, which now contains both proxy and management settings. Standalone mode uses `-alone -f sgate-client.cf` or `-alone -f sgate-server.cf`.

The two production configurations use `master_type = sock`; the five application-specific examples have moved to `samples/`. Keep `master_maxproc = 1`, `master_prefork = 1`, `master_reuseport = no`, `master_status_notify = 0`, `master_unpriv = n`, and `master_chroot = n`. Set `master_stop_kill = false` and `master_stop_wait = true` for status-channel shutdown. Credentials, certificates and application addresses are preserved. Process identity is established by acl_master or the service account; in-template privilege changes/chroot are rejected.

`master_service` owns the effective listening addresses, including precedence over application `listen`/`-master_service`. Other CLI overrides still win over file values. Web apply keeps the process PID, management login and byte counters while replacing the proxy worker. Changing the effective application listen address is rejected with file rollback; change `master_service` and stop/start the service to change ports. Both framework and Web proxy restarts interrupt existing business connections. Relative application paths use `master_home`; management paths remain relative to the management configuration.

When migrating from `master_type = none`, stop the old service first, install the new executable/configuration, then start through `master_ctl`. The integration test below runs an isolated framework with random loopback ports and never loads installed production service configurations:

```sh
make build
python3 scripts/test-acl-master.py --merged --client bin/sgate-client --server bin/sgate-server --output artifacts/acl-master-test
```

Use `--master` and `--ctl` for other framework installations. The same test accepts race-instrumented binaries and covers WS/WSS, HTTP/HTTPS management, two listeners per role, 32-stream integrity, Web apply, persistent login and framework stop/start/restart.

For managed stress testing, build disposable diagnostic binaries through Go overlays:

```sh
python3 scripts/build-config-soak.py --managed --output artifacts/acl-stress/plain
python3 scripts/test-acl-master.py --merged --stress --concurrency 128 --rounds 6 \
  --client artifacts/acl-stress/plain/sgate-client \
  --server artifacts/acl-stress/plain/sgate-server --output artifacts/acl-stress/plain-results
```

Add `--race` when building and point the test at the generated `-race` binaries for race detection. Each WS/WSS case uses default reconnect backoff, random padding (10–30ms, 128–512 bytes), 128 concurrent streams and alternating client/server framework restarts. Transport interruptions must preserve stream bytes; process restarts close old streams and must preserve login sessions and allow fresh streams. The test waits for the configured 20-second old-session retention window before checking release, records idle memory/goroutine/descriptor/cache samples, and checks shutdown censuses, child exits and port rebinding. Diagnostic sampling is absent from production binaries.

The standalone regression checks both executables with CLI-only options, explicit `-alone`, configuration files and CLI overrides, including WS/WSS integrity and listener release:

```sh
SGATE_ADMIN_CLIENT_BINARY="$PWD/bin/sgate-client" SGATE_ADMIN_SERVER_BINARY="$PWD/bin/sgate-server" \
  go test ./internal/app -run '^TestStandaloneStartModesBinaries$' -v -count=1 -timeout=2m
```

## Web management

Both programs support optional Web management. The client uses a separate listener; the server shares its proxy listener at `/admin/`. Enable it alongside the application file being managed:

```sh
./bin/sgate-client -config sgate-client.cf -admin-enabled
./bin/sgate-server -config sgate-server.cf -admin-enabled
```

The HTML, CSS and JavaScript are external files in `web/admin/`; they are not embedded in the Go binaries. Deploy the executable, `.cf` and `web/`, retaining `conf/` for account/session data. The merged examples set `admin_web_dir = web/admin`, relative to the `.cf`; absolute paths are also supported. Both programs can share one UI directory or use separate ones. Editing/replacing these files takes effect on browser refresh without rebuilding or restarting the proxy. All four files (`index.html`, `app.css`, `i18n.js`, `app.js`) must exist at startup. Use atomic file replacement when deploying updates.

The process needs search permission on UI directories, read permission on UI files, and write permission on the application configuration's parent directory for validation, backups and atomic replacement. Startup errors preserve the affected path and underlying filesystem error. Missing or unreadable assets at runtime return `503`; authenticated APIs and existing sessions remain available, and restoring the files or permissions restores the page without a restart. A non-writable configuration directory rejects saves without changing the file or running proxy.

Set the application addresses, key and proxy password first. Client management defaults to `http://127.0.0.1:9081/`; `/admin` redirects to `/admin/`, and both paths share the root interface’s accounts and login sessions. Server management shares the proxy listener at `/admin/`, for example `https://host:8080/admin/` alongside `wss://host:8080/tunnel`; it remains disabled unless `admin_enabled = true` or `-admin-enabled` is supplied. The first visit prompts for an administrator account. Obtain the generated initialization token from the `setup-token-file` path printed in the local startup log; the token itself is never logged. Enter a username, a password of at least 6 characters, and the token in the setup form. Successful setup removes the token. No default account is shipped. The separate `users-file` stores only a salted PBKDF2-HMAC-SHA256 hash with 600,000 iterations; it does not reuse the tunnel password.

The [client example](../sgate-client.cf) and [server example](../sgate-server.cf) contain the management settings in the same service block. Prefix the following legacy management option names with `admin_` and replace hyphens with underscores: `listen`, `web-dir`, `users-file`, `session-dir`, `session-timeout` (default `12h`, fixed absolute expiry), `max-sessions` (default 128), `setup-token-file`, and optional `tls-cert`/`tls-key`. Paths inside management configuration resolve relative to that file's directory, unlike application paths which keep resolving against the process working directory. The session directory must be private (`0700`); generated credential, session, token and backup files use `0600`. Server shared management allows HTTP/WS on any listen address by default; configure the proxy tls-cert/tls-key to use HTTPS/WSS. Client management also allows HTTP on any listen address. A complete admin_tls_cert/admin_tls_key pair selects HTTPS; leaving both empty selects HTTP. An incomplete pair or unreadable certificate/key prevents startup. The server uses the proxy listener at `/admin/` and the proxy TLS certificate/key, with no separate management port option. Standalone `listen` / `-master_service` or managed `master_service` determines the port. Legacy `admin_listen` is ignored, excluded from server configuration forms and no longer used for login identity. Valid old sessions automatically migrate to the new cookie name without extending their expiry. Legacy `admin_tls_cert`/`admin_tls_key` are not loaded. Client management retains its independent listener. Cookies are persistent, HttpOnly and SameSite=Strict, with Secure on HTTPS. Login survives process restarts until the original expiry; activity does not renew it. Expiry rejects requests immediately and background cleanup runs at most one minute later. Logout revokes the stored session, and changing the administrator credentials requires the current password and revokes all sessions. Changes to management storage/timeout settings and client management listener/TLS require a full process restart. Server proxy certificates can be reloaded with proxy apply; new TLS connections use the replacement certificate. Shared listener address or HTTP/HTTPS mode changes require save-only followed by a full process restart; apply rejects and rolls back those changes.

The configuration page presents one form row per setting in the selected `-config` file, grouped into framework, proxy/tunnel and Web management options. Each row shows its file comment and any CLI override. Each row has a save icon, initially gray and disabled. Editing enables it; reverting or saving disables it again. Clicking persists only that setting without restarting the proxy or discarding drafts in other rows. Full-form save and apply remain available. Saves replace only edited values, retaining comments and untouched file content. Secret fields (`password`, `master_args`, `master_env`) use empty password inputs: leave them untouched to keep the existing value, or enter a replacement. Raw file content is not displayed. Saves validate syntax, effective proxy settings and key/certificate files, check a content revision, back up the old file to `.bak`, and replace it atomically. **Save and restart proxy** applies changes within the process while keeping the management listener and login alive. Existing local application connections close; the peer follows the existing retention rules. Explicit CLI overrides remain in effect and their names are shown. Failed new listener startup attempts to restore the previous running proxy; a failed save-and-restart also restores the prior on-disk file. Interrupted application connections cannot be restored by this rollback.

The dashboard shows numeric TX/RX rates, cumulative bytes, connection counts and a 60-sample chart. Counts cover tunnel TCP bytes including HTTP/WS/TLS, padding and tunnel replay, not TCP/IP headers, kernel TCP retransmission packets, local application sockets or other host interfaces. The shared server listener includes management HTTP/TLS traffic; separate client management traffic is excluded. Rates update each second. Counters survive an in-process proxy restart and reset on a full process restart.

To run the external UI deployment matrix against built binaries:

```sh
SGATE_ADMIN_CLIENT_BINARY="$PWD/bin/sgate-client" \
SGATE_ADMIN_SERVER_BINARY="$PWD/bin/sgate-server" \
go test ./internal/app -run '^TestManagementDeploymentBinaries$' -v -count=1 -timeout=4m
```

The test uses temporary deployment trees and loopback listeners, with both HTTP and HTTPS, varied working directories, relative/absolute UI paths, permission failures and recovery. The variables can also point to race-instrumented binaries. Unix permission-denial checks are skipped when running as root.

## Merged configuration deployment regression

For shared-port management stress, add `--management-stress --login-concurrency 8 --apply-rounds 6` to `scripts/test-acl-master.py --merged --stress --concurrency 128 --rounds 3 ...`. Add `--mode standalone` for direct startup; the default uses a real acl_master. Both WS/WSS run unless `--tls wss` is selected. Each round overlaps 128 traffic workers, login bursts, management polling, random padding changes, repeated save/apply, transport faults and process replacement. Accepted cookies must survive every round; expected login throttling is counted separately. `management-load-*.json` records request latency, outcomes and integrity counters without credentials or cookies. Proxy/process restarts deliberately close old application streams; the mixed phase checks all received record prefixes and completed records, while separate fault-only waves require complete payload delivery without duplication.

Build diagnostic binaries with `python3 scripts/build-config-soak.py --managed --output artifacts/merged-deployment/plain`, then run `python3 scripts/test-merged-deployment.py --client artifacts/merged-deployment/plain/sgate-client --server artifacts/merged-deployment/plain/sgate-server --output artifacts/merged-deployment/plain-results`. Add `--race` to the build and use the `-race` binaries for race detection.

The eight cases cover standalone/isolated acl_master, WS/WSS and relative/absolute paths. They seed accounts and cookies using the legacy split files, remove those files, migrate to `.cf`, and restart from different working directories at a different depth from configuration files. The matrix checks original cookies and password login, CLI overrides, 32-stream integrity, configuration save/apply, restart, idle/closed resource censuses and port rebinding. Descriptor inventories ensure workers do not retain master control sockets. Use `--mode managed --tls ws --paths relative --restarts 3` for repeated managed restart checks. Unix startup closes unrelated inherited sockets lacking CLOEXEC, preserving the reserved state/listener descriptors and Go-owned sockets, files and pipes.

## Application configuration files

Both programs accept `-config FILE`. Every existing runtime flag can be set using its name without the leading dash. Precedence is **explicit command-line flags > configuration file > built-in defaults**, including explicit zero and empty values. The framework `-f` file is used when `-config` is omitted; without either flag, startup uses CLI options only.

Complete examples: [client](../sgate-client.cf) and [server](../sgate-server.cf). Set your password, addresses and key paths before running:

```sh
./bin/sgate-server -config sgate-server.cf
./bin/sgate-client -config sgate-client.cf -master_service 127.0.0.1:2901
```

Files use UTF-8 `key = value` lines. Blank lines and full-line `#` / `;` comments are supported. Durations use Go syntax (`250ms`, `2m`); sizes use integer bytes. Unquoted values are trimmed, single quotes preserve literal content, and double quotes accept Go string escapes. Native `.cf` files use the template Config.InitConfig/GetString API and support whitespace followed by `#` for inline comments; quote literal `#` in secrets. Legacy flat `.conf` files retain their original comment rules. Unknown or duplicate keys, malformed syntax, invalid effective values and unreadable files fail startup. File values overridden by flags are not converted. Relative paths resolve against the process working directory. `SGATE_KEY` remains the fallback when no key file is supplied.

Overrides apply separately to each option. To disable a configured random padding interval, pass `-noise-interval-min 0 -noise-interval-max 0`; to switch to fixed scheduling, also pass `-noise-interval 1s`. Existing interval validation still applies. The service `.cf` combines framework, proxy and `admin_` settings; `master_args` may be empty or contain CLI overrides. Managed startup automatically reads the framework's `-f` file. Restrict access to files containing passwords, for example with `chmod 600`.

The five SSH/VPN/Web `.cf` files preserve existing application values. Generic examples `sgate-client.cf` and `sgate-server.cf` have `master_disable = yes`, which blocks service loading by acl_master, including both autostart and master_ctl start. Set `master_disable = no` in the deployed configuration before managed startup; standalone startup is unaffected. The old shipped `conf/*.conf` files have been merged and removed; readers remain backward compatible. Management remains disabled by default. Users, sessions and tokens remain separate data files. Their paths and `admin_web_dir` are relative to the installed `.cf`, so use absolute paths when relocating service files to retain existing login state. Application paths still use the working directory. Stop old services before upgrading binaries and configuration. The Web client password remains empty until supplied at deployment.

Framework and management settings require a full process restart: Web save-only is allowed, but proxy-only apply rejects and rolls back these changes. Application options still support Web proxy restart. Protect service files with mode 0600 and enable directory write access only where Web saving is needed. Use `python3 scripts/test-acl-master.py --merged ...` to run the isolated single-file integration matrix.

## Build

Requires Go 1.24 or newer and Make. Normal builds need neither Python, a C compiler nor acl_master; race tests require a C compiler.

```sh
make build
make client                       # client only
make server                       # server only
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 make build BINDIR=dist/linux-amd64
make test
make help
```

The default build explicitly uses module mode and downloads missing dependencies according to `go.mod` and `go.sum`. Set `GOPROXY` for your network, or run `make deps` to prefetch dependencies. Both `third_party/gin-service/` and `third_party/go-service/` contain required local lifecycle extensions and must be committed and cloned with the project. Upstream releases cannot replace them. An existing `vendor/` is not required and will not override these sources.

Override `GO`, `BINDIR`, `BUILD_FLAGS`, `LDFLAGS` or `TEST_FLAGS` as needed. `make framework-test` tests both local framework modules. `make -f /absolute/path/to/sgate/Makefile build` also works from another directory; relative output directories are resolved against the project root. `make clean` removes only the two generated programs.

For an offline build, first run `make vendor-sync` on a connected machine (requires Python 3), copy the complete generated vendor directory with the source, then run `GOPROXY=off make build MOD=vendor`. Synchronize again after changing local framework sources.

Install to `/opt/soft/sgate` with `make build` as your normal user, followed by `sudo make install`. Installation does not invoke Go, download dependencies or rebuild programs, so sudo does not need a Go environment. By default both programs are installed. Use `sudo make install server` to install only the server, or `sudo make install client` for only the client, including matching configuration, scripts and examples. Only the selected executable(s) must already exist and be executable; otherwise installation stops before writing the destination and asks you to build as a normal user. In these install commands, `server` and `client` are selectors and do not trigger compilation; standalone `make server` and `make client` still build the corresponding program. Shared Web assets and keys follow the existing installation rules; an existing installation of the other role is preserved. `make install-files` remains an equivalent compatibility alias. If you override `BINDIR`, use the same directory for building and installing. Override `PREFIX` for another runtime location; `DESTDIR` stages a package without changing the runtime paths written to configuration.

Programs go in `sbin/`, the four general service control scripts in `bin/`, two production service configurations and the shared key in `conf/`, and management assets in `web/admin/`. The installer also creates `logs/` and `var/` and copies the program/framework licenses. New configurations have their installation paths relocated while preserving application settings. Existing configurations, keys and login state are retained; updated templates are installed as `.cf.dist`. Newly installed configurations and keys use mode `0600`. Set appropriate directory ownership when using a separate service account.

Local administrator accounts, sessions and setup tokens are not copied. An existing source `sgate.key` is copied only when the destination has no key; otherwise a new 32-byte random key is generated. Both endpoints must use the same key. External certificates and acl_master must be configured separately. Installation does not start or register services. The installed `sgate-client-start.sh`, `sgate-client-stop.sh`, `sgate-server-start.sh` and `sgate-server-stop.sh` scripts read their corresponding production configuration in `conf/` and support a `MASTER_CTL` override. SSH, VPN and Web examples and their legacy SSH scripts live in `samples/` in both the source tree and installation. Existing legacy deployment files are preserved during upgrades; stop and migrate old services before archiving them.

For standalone startup, use `/opt/soft/sgate/sbin/sgate-server -alone -f /opt/soft/sgate/conf/sgate-server.cf` (and the client equivalent) after configuring addresses and passwords.

## Run

Generate a random shared key and securely copy the same key file to both endpoints:

```sh
umask 077
openssl rand -hex 32 > sgate.key
```

On the server:

```sh
./bin/sgate-server -master_service :8080 -target 127.0.0.1:5432 -key-file sgate.key -password 'your-password'
```

On the client:

```sh
./bin/sgate-client -master_service 127.0.0.1:15432 \
  -server ws://SERVER_IP:8080/tunnel -key-file sgate.key -password 'your-password'
```

Connect your application to `127.0.0.1:15432`. Alternatively, supply the 64-character hexadecimal key through `SGATE_KEY`.

Both endpoints require the same `-password` value (1–1024 bytes). Replace `your-password` in the examples. The password is separate from `sgate.key`.

The client includes an `X-Sgate-Password` header in every HTTP/HTTPS WebSocket handshake, including reconnects. It contains a versioned Base64URL encoding of a fresh nonce and AES-256-GCM ciphertext, using a dedicated encryption key derived from the shared key. Authentication binds the encrypted credential to the session, request nonce, timestamp, resume flag and URI. Request and response proofs also bind the credential.

The server decrypts and verifies the password before upgrading or allocating a proxy session. Missing, incorrect, tampered or duplicated credentials return HTTP 404 without creating a proxy session or backend connection. Replayed handshakes are also rejected within the running server's in-memory replay window. Passwords are required at startup and are never sent as plaintext headers. The ordinary HTTP homepage remains public. Upgrade both programs together: older key-only handshakes are no longer accepted.

For public deployments use `wss://`. The server supports `-tls-cert` and `-tls-key`; the client verifies certificates and supports additional trust roots with `-tls-ca`. A WebSocket-capable HTTPS reverse proxy can terminate TLS instead.

## Random traffic padding

Both programs accept `-noise-interval 1s -noise-min-bytes 1024 -noise-max-bytes 10240` to schedule one encrypted padding message per second, with a uniformly random payload size of 1–10 KiB inclusive. Sending is disabled by default (`-noise-interval 0`). The default size bounds are 1024 and 10240 bytes; valid bounds satisfy `1 <= min <= max <= 16384`. Each direction is configured independently; a peer with sending disabled still discards incoming padding. For acl-master `.cf` deployments, append these flags to `master_args`.

For random intervals, replace the fixed interval with `-noise-interval-min 1s -noise-interval-max 3s`. Both bounds are required, must satisfy `0 < min <= max`, and cannot be combined with a nonzero `-noise-interval`. The initial wait and the wait after each completed send are independently sampled uniformly from the inclusive range at nanosecond resolution; payload sizes remain independently random. Equal bounds give a fixed wait. Congestion can extend actual intervals. Reconnects choose a fresh initial wait, and overdue messages are never accumulated.

Padding is sent while the WebSocket is connected, including when no application streams exist. Receivers decrypt and validate it, then discard it without forwarding bytes, opening backend connections, changing reliable sequence numbers, or filling replay buffers. Padding still uses the transport encryption counter. Timers stop on disconnect and restart on reconnect; missed intervals and old padding are never queued for replay. Application frames get a turn before due padding, and heartbeats remain scheduled. Congestion can delay transmission. Upgrade both programs before enabling padding: older peers reject the new frame type.

Sizes refer to payload bytes, excluding the 25-byte tunnel header, 24-byte encryption overhead, and WebSocket/TLS/TCP/IP overhead. WebSocket messages do not map directly to network packet sizes. The example adds an average 5.5 KiB/s of payload per enabled direction, per online session. Padding consumes bandwidth and does not guarantee resistance to traffic classification or conceal application timing and sizes.

## Recovery

The client reconnects automatically with exponential backoff and jitter. Both endpoints retain TCP sockets and unacknowledged frames across temporary WebSocket outages. Sequence numbers, cumulative acknowledgements, and deduplication preserve byte order without repeating delivery. TCP half-close is supported.

The default disconnected-session retention is two minutes (`-session-ttl`). Restarting either process, session expiry, backend disconnects, or application timeouts cannot preserve existing application sockets. On session loss, old application connections close and the client starts a new session for subsequent connections. An ACK confirms forwarding to a TCP socket, not completion of a business transaction.

The client also expires offline connections and replay buffers after two minutes (`-offline-timeout`), even if the network never returns. It keeps retrying with a fresh session. This also bounds application connections accepted before the initial tunnel handshake; a stalled handshake or reconnect backoff cannot extend retention. Healthy online sessions are unaffected. Adjust both client and server retention when extending the recovery window. For silent packet loss, offline retention starts after transport failure is detected, for example by heartbeat timeout.

Both programs expose `-heartbeat` (default `20s`) and `-heartbeat-timeout` (default three heartbeat intervals, or `60s`). Slow payload reads refresh the receive deadline before a whole message completes; local TCP processing does not consume the next read's idle window, and a busy send queue cannot starve probes. Ping replies use the configured write budget. For high-latency links, configure a sufficient receive window on both endpoints, for example `-heartbeat 10s -heartbeat-timeout 90s`. The window must exceed the heartbeat, dial and write budgets and accommodate expected quiet periods, jitter and retransmission delays; exceeding it still triggers reconnection.

Session-expiry HTTP 410 responses must carry a valid server signature. An unsigned 410 from an intermediary triggers a retry without discarding live connections. Upgrade both endpoints together: older servers send unsigned expiry responses that newer clients will reject.

Each direction buffers at most 4 MiB of unacknowledged frames by default (`-buffer-bytes`), then applies TCP backpressure. The default limits are 128 streams per session (`-max-streams`) and 128 retained sessions on the server (`-max-sessions`). Socket buffers and per-stream read buffers consume additional memory. Slow backend writes can temporarily delay other streams in the same session.

Transport messages and controls are encrypted even with `ws://`; HTTP upgrade headers remain visible without TLS. Standard HTTP/WebSocket compatibility does not guarantee resistance to traffic classification. The ordinary HTTP root returns a small page.

## Verification

```sh
go test -race ./...
go vet ./...
```

Tests cover tampering, replay, authentication failures, multiplexing, half-close, reconnect with a 2 MiB payload on the original backend socket, lost acknowledgements in both directions, eight successive disconnects during independent bidirectional transfers, bounded buffering, blocked producer cancellation after reset, storage release after acknowledgement, session cleanup, expiration, and backend failure.

Recovery tests fill both replay queues, restart the server on the same port, repeat five restarts, simulate silent downstream packet loss and stalled handshakes, and verify that new TCP streams never receive stale data. Run the real two-minute offline window with:

```sh
SGATE_RECOVERY_LONG=1 go test -race \
  -run '^TestLongRecoveryDefaultOfflineWindow$' -v -timeout 3m ./internal/tunnel
```

For recovery with random padding enabled, run:

```sh
go test -race -count=3 \
  -run '^(TestNoiseRecoveryScenarios|TestNoisePendingTimerReleasedOnExpiryAndShutdown)$' \
  -v -timeout 1m ./internal/tunnel
SGATE_RECOVERY_LONG=1 go test -race \
  -run '^TestNoiseLongRecoveryDefaultOfflineWindow$' -v -timeout 3m ./internal/tunnel
```

The HTTP/HTTPS matrix covers full replay buffers during outages, same-port server instance restarts, five repeated restarts, server expiry and client offline expiry. Fresh streams must deliver exact bytes and EOF without stale data, and fresh transports must resume padding. Checks join the old transport and writer, proving the deferred padding timer cleanup completed; old padding counters remain frozen during the real two-minute HTTPS outage with 1–3 second waits and 1–10 KiB payloads. A 1–2 hour initial timer also verifies that expiry and shutdown do not wait for the timer to fire. Goroutines and descriptors must return to baseline after cleanup.

## Resource stress test

```sh
make stress
SGATE_STRESS_DURATION=3m GOMAXPROCS=4 make stress
SGATE_STRESS_NOISE_INTERVAL=5ms make stress
SGATE_STRESS_DUPLEX=1 SGATE_STRESS_DURATION=3m \
  SGATE_STRESS_NOISE_INTERVAL_MIN=1ms SGATE_STRESS_NOISE_INTERVAL_MAX=5ms \
  SGATE_STRESS_NOISE_MIN_BYTES=1024 SGATE_STRESS_NOISE_MAX_BYTES=10240 \
  GOMAXPROCS=4 make stress
SGATE_STRESS=1 GOMAXPROCS=4 go test -race \
  -run '^TestStressReconnectResources$' -v -timeout 10m ./internal/tunnel
```

The default workload uses four clients and 64 TCP streams per client, cycling through 12 rounds with 128 KiB exchanges and an injected outage every 30 ms. Each outage blocks reconnects for 10 ms. When a duration is configured, each batch keeps its sockets open for repeated exchanges before half-closing and cycling connections.

`SGATE_STRESS_CLIENTS`, `SGATE_STRESS_STREAMS`, `SGATE_STRESS_ROUNDS`, `SGATE_STRESS_BYTES`, and `SGATE_STRESS_DURATION` control the workload. `make stress` saves JSON samples to `artifacts/stress.json`; override with `SGATE_STRESS_REPORT`. Direct `go test` invocation only saves a report when that variable is set.

`SGATE_STRESS_DUPLEX=1` sends independent pseudorandom streams in both directions. Record sequences, byte comparisons, a required terminal marker and EOF detect missing/repeated bytes and truncated tails. Sender and receiver totals are checked independently and reported as `verified_upload_bytes` and `verified_download_bytes`; the default echo mode uses `verified_bytes_per_direction`. Samples include client streams and replay buffers as well as server resources, and actual padding send/receive counts on both peers. Padding may be discarded on disconnect, so its counts need not match.

Independent duplex mode bounds load-generator socket buffers to 16 KiB and waits two seconds between records on each sending stream. Override with `SGATE_STRESS_SOCKET_BUFFER` (`0` uses OS defaults) and `SGATE_STRESS_RECORD_INTERVAL`. Running all application and proxy endpoints on one machine amplifies kernel buffer pressure: unpaced 256-stream, 128-KiB bursts can exhaust macOS networking memory before testing tunnel correctness. Bounded buffers and offered load preserve stream count, record size and outage frequency without changing system settings. On macOS, `network_memory_denied` records the system-wide cumulative allocation-failure counter (`-1` if unavailable); compare deltas and consider other processes. A prolonged stall saves a `.stacks` diagnostic beside the report.

Set random wait bounds with `SGATE_STRESS_NOISE_INTERVAL_MIN` / `SGATE_STRESS_NOISE_INTERVAL_MAX`, and payload bounds with `SGATE_STRESS_NOISE_MIN_BYTES` / `SGATE_STRESS_NOISE_MAX_BYTES`. `SGATE_STRESS_CHURN_INTERVAL` and `SGATE_STRESS_OFFLINE_DURATION` override the default `30ms` / `10ms` outage schedule. To test 1–3 second waits, use a churn interval such as `4s`: reconnecting every 30ms continuously restarts the initial wait and cannot exercise that padding schedule.

Samples include heap, stack, goroutines, file descriptors, accepted TCP connections, retained sessions, replay buffers, and authentication nonce counts. Idle checkpoints force GC and also record RSS (`ps`); macOS descriptor counting uses `lsof`. Runtime pages and race-detector shadow memory can remain resident, so assess RSS trends together with retained heap and resource counts. Nonces expire within a two-minute retention window. Assertions check idle recovery and shutdown cleanup. Concurrent error reports for the same stream produce only one RESET, preventing duplicate notifications from exhausting control work and closing unrelated streams.

## Weak-network tests

Run `make weaknet` for three race-enabled repetitions. A bounded local TCP relay models 150ms one-way delay, ±100ms jitter, 15% delayed segment retransmissions at 200ms, slow payload delivery and short/long blackholes in each direction, including TLS transport coverage. It preserves ordered TCP delivery: synthetic losses delay retransmission rather than remove bytes from a TCP stream. It does not modify NIC or kernel packet-loss rules. Size the receive timeout to include complete TLS record delivery on TLS links.

Checks cover idle heartbeat stability, random byte-for-byte exchanges, exact backend byte counts, half-close tails, resume on the original application socket, expiry and fresh connections. Already-delivered bytes can remain in the old application's receive buffer before EOF/reset. Relay connections, queued bytes, application sockets, replay queues and goroutines must all be released during cleanup.

`make weaknet` also covers randomized padding over HTTP and HTTPS, with 1–10 KiB payloads. Dense tests use 100–200ms waits; the deliberately slow relay uses 300–600ms to keep padding within its capacity. Independent duplex cases offer about 40 KiB/s per direction with 200–400ms padding waits. Separate cases exercise real 1–3 second waits and heartbeat fairness under a busy writer with 1–3ms padding. Independent duplex senders use different random seeds, sequence records, terminal markers and EOF to detect missing/repeated bytes and truncated tails. Healthy transports must not reconnect; short directional stalls preserve the transport, longer stalls resume the original application socket, and expiry releases old state. Checks prove both peers receive padding during impairment and join transport writers on shutdown, after their padding timers and heartbeat tickers stop. Goroutines and descriptors must return near baseline. The slow TLS case allows a one-second heartbeat timeout for complete TLS record delivery.

Run just the padding cases with `go test -race -count=3 -run '^TestNoiseWeakNetwork' -v -timeout 6m ./internal/tunnel`.

### Combined long-running weak-network stress

Combine 256 application connections, independent duplex streams, an outage every four seconds, randomized padding and weak-network impairment for six rounds and at least ten minutes of offered load:

```sh
SGATE_STRESS=1 SGATE_STRESS_DUPLEX=1 SGATE_STRESS_WEAKNET=1 \
SGATE_STRESS_CLIENTS=4 SGATE_STRESS_STREAMS=64 SGATE_STRESS_ROUNDS=6 \
SGATE_STRESS_DURATION=10m SGATE_STRESS_BYTES=1024 \
SGATE_STRESS_CHURN_INTERVAL=4s SGATE_STRESS_OFFLINE_DURATION=100ms \
SGATE_STRESS_NOISE_INTERVAL_MIN=200ms SGATE_STRESS_NOISE_INTERVAL_MAX=600ms \
SGATE_STRESS_REPORT="$PWD/artifacts/noise-combined-long.json" \
go test -run '^TestStressReconnectResources$' -v -count=1 -timeout 15m ./internal/tunnel
```

Defaults model 150ms one-way delay, ±100ms jitter and 15% synthetic segment retransmissions delayed by 200ms, preserving ordered TCP bytes without injecting actual IP loss. Override with `SGATE_STRESS_WEAK_DELAY`, `SGATE_STRESS_WEAK_JITTER`, `SGATE_STRESS_WEAK_LOSS_PERCENT` and `SGATE_STRESS_WEAK_RETRANSMIT`. Duplex mode sends a record every two seconds per connection; 1 KiB records keep offered application traffic plus padding within the simulated link's capacity.

Reports include relay connections, queued bytes, accepted connections, retransmissions and idle/shutdown transport-writer counts. Each idle checkpoint requires empty application streams and replay queues, released old relay connections, and bounded goroutines/descriptors. Shutdown joins both peers' transports, after their padding timers and heartbeat tickers stop, and requires zero relay connections and queued bytes. Heap samples include the test's growing report array; interpret them with RSS, proxy state and shutdown results. Add `-race` for a separate concurrency check; compare its RSS separately because of race-detector overhead.

Set `SGATE_STRESS_TLS=1` for HTTPS/WSS with a trusted test certificate and hostname/chain verification. `SGATE_STRESS_LIFECYCLE=1` requires independent duplex mode and at least two rounds. It alternates server-instance restart and session-expiry phases between rounds, leaving the final round to verify recovered application traffic:

```sh
SGATE_STRESS=1 SGATE_STRESS_DUPLEX=1 SGATE_STRESS_TLS=1 \
SGATE_STRESS_WEAKNET=1 SGATE_STRESS_LIFECYCLE=1 \
SGATE_STRESS_CLIENTS=4 SGATE_STRESS_STREAMS=64 SGATE_STRESS_ROUNDS=6 \
SGATE_STRESS_DURATION=10m SGATE_STRESS_BYTES=1024 \
SGATE_STRESS_CHURN_INTERVAL=6s SGATE_STRESS_OFFLINE_DURATION=100ms \
SGATE_STRESS_NOISE_INTERVAL_MIN=300ms SGATE_STRESS_NOISE_INTERVAL_MAX=700ms \
SGATE_STRESS_REPORT="$PWD/artifacts/wss-combined-long.json" \
go test -run '^TestStressReconnectResources$' -v -count=1 -timeout 16m ./internal/tunnel
```

Six rounds include three restarts and two expiries. Each failure phase creates another full application cohort, blocks delivery and fills both peers' replay queues. Restart stops a server with live transport writers, closes its HTTPS listener and starts a new instance on the same address with the same certificate. Expiry uses the test's 10-second retention period, requiring server cleanup while the client retains pending data until an authenticated expiry response. Old application connections, sessions, caches and transport timers must be released before recovery is accepted. `intentionally_interrupted_streams` counts these expected terminations separately from fully verified normal duplex byte totals. Old streams use independently seeded pseudorandom data. Delivered bytes must be an exact prefix; verifier counterexamples cover missing/repeated bytes and overlong data, and read timeouts fail. New seeds and sequence records in subsequent rounds detect stale data replayed into new streams. Wall time also includes expiry waits, TLS reconnects and cleanup. Add `-race` for a separate concurrency run.

### WSS outages with the default two-minute retention

`SGATE_STRESS_DEFAULT_RETENTION=1` leaves server retention and client offline timeout unset, exercising their real two-minute defaults. For recovery before expiry, combine the TLS/weak-network/duplex options above with `SGATE_STRESS_DEFAULT_RETENTION=1 SGATE_STRESS_RETENTION_OUTAGE=110s SGATE_STRESS_ROUNDS=1 SGATE_STRESS_DURATION=20s SGATE_STRESS_CHURN_INTERVAL=6s`. Use four clients, 64 streams each, 1 KiB records and 1–3 second random padding waits, and allow a five-minute test timeout. Do not set lifecycle mode for this case.

Both peers must retain their original sessions and application connections, fill bounded replay queues, stop padding while detached and then deliver exact duplex records, terminal markers and EOF after recovery. Application deadlines include the planned outage, and intentional offline time does not trigger a stalled-transfer diagnostic. Reports include `retention_outage` and `retained_session_continuity`.

For expiry, use `SGATE_STRESS_DEFAULT_RETENTION=1 SGATE_STRESS_LIFECYCLE=1 SGATE_STRESS_ROUNDS=2 SGATE_STRESS_DURATION=24s SGATE_STRESS_CHURN_INTERVAL=1m`, with the same TLS, weak-network, padding and concurrency settings and no retention-outage override. Default-retention lifecycle mode injects expiry rather than restart. It keeps the network blocked until all old sessions, sockets, replay queues and transport timers are released near the real two-minute deadline; the server reaper and client timeout may run in either order. The `expiry-offline-expired` checkpoint precedes network recovery. A new cohort then verifies fresh sessions and absence of stale data. The 256 intentionally terminated probe streams are counted separately from fully verified normal traffic. Use an absolute `SGATE_STRESS_REPORT` path and run `go test -race -run '^TestStressReconnectResources$' -v -count=1 -timeout 5m ./internal/tunnel`.

### Recovery at 119, 120 and 121 seconds

Set `SGATE_STRESS_BOUNDARY_OUTAGE=119s`, `120s` or `121s` with WSS, weak-network, default-retention, duplex and lifecycle modes, four clients, 64 streams each and two rounds. Use 1 KiB records, 24 seconds of normal workload, a one-minute churn interval, 1–3 second padding waits and a six-minute test timeout. The exercised profile uses 75ms one-way delay, ±50ms jitter and 15% retransmissions delayed by 100ms. Each scenario establishes a full old-stream cohort with filled queues between normal rounds.

`boundary_restore_milliseconds` records network restoration after detachment; `boundary_resolve_milliseconds` includes subsequent TLS/HTTP handshakes, transfer and cleanup. Outcomes are classified per client: a resumed session must preserve identity and deliver every old stream exactly, including half-close/EOF; an expired session must release both peers' old sockets, replay queues and transport timers, accepting only correct delivered prefixes. Orphaned backend connections, caches or extra server sessions fail. Fresh traffic in the next round checks cross-stream/stale-data isolation. Network restoration does not guarantee that the authenticated resume handshake finishes before retention expires, so the test accepts either valid outcome at the boundary. Reports include resumed/expired session and complete/interrupted old-stream counts.

Verifier byte streams now use PCG with full 64-bit seeds covering round, client and stream IDs. A regression case detects cross-client collisions caused by the legacy generator reducing high-bit seeds, and checks consistency across split reads.

The server confirms a resume upon the first valid encrypted frame from the client. Failed upgrades, abandoned upgrades and silent unconfirmed connections do not renew the old retention timestamp. Unconfirmed reads and Ping/Pong progress remain capped at the original retention deadline. After confirmation, normal heartbeat handling applies and disconnect starts a fresh retention window. `TestResumeConfirmationControlsRetention` covers abandoned, silent and confirmed resumes.


To stress unconfirmed resumes with the real default window, use the default-retention/WSS/weak-network/duplex/lifecycle settings above without `SGATE_STRESS_BOUNDARY_OUTAGE`, and run `SGATE_STRESS_UNCONFIRMED=tls-timeout`, `abandoned` and `silent` separately. Use 64 clients, four streams each, two rounds, 24 seconds of normal workload and a six-minute timeout. Timeout and abandoned-upgrade probes run at the start, midpoint and one second before the original deadline. Silent upgrades stay open; half only read server padding and reply to WebSocket Ping, without sending encrypted protocol frames. Every old retention timestamp must remain unchanged, both peers must release old streams, queues and transport timers at the original deadline, and fresh traffic must pass after recovery. Reports record actual probe counts, received padding and `unconfirmed_release_milliseconds`. The optional `SGATE_STRESS_UNCONFIRMED_TTL=6s` shortens both peers' test deadlines for a smoke run only; omit it when testing the default window.


### Continuous control frames and incomplete encrypted messages

Run the unconfirmed-resume profile above with `SGATE_STRESS_UNCONFIRMED=control` and `fragmented` separately, using 64 clients, four streams each, two rounds and the real default two-minute window. Control probes send Ping and Pong every 250ms. Fragment probes send eight ciphertext bytes per 250ms in masked WebSocket frames: binary first, then continuation frames, all without FIN. The valid encrypted message remains incomplete and lacks its authentication tag. Both modes drain server padding and reply to server Ping. Premature probe termination fails the test.

Every original retention timestamp must remain unchanged; replay queues must remain bounded; old connections and transport timers must be released at the original deadline; fresh independent duplex traffic must pass after recovery. Reports add `unconfirmed_control_sent`, `unconfirmed_pongs_received`, `unconfirmed_fragments_sent` and `unconfirmed_partial_bytes`. Both modes also run as short-window cases in `TestResumeConfirmationControlsRetention`. Omit `SGATE_STRESS_UNCONFIRMED_TTL` for full default-window stress runs.


### Serial macOS network-memory diagnosis

`netstat -m` reports a system-wide cumulative allocation-failure count, not per-process leaks or lost application bytes. Compile both test binaries before measuring, then run the serial comparison:

```sh
mkdir -p artifacts
GOMAXPROCS=4 go test -c -o artifacts/network-memory-plain.test ./internal/tunnel
GOMAXPROCS=4 go test -race -c -o artifacts/network-memory-race.test ./internal/tunnel
python3 scripts/diagnose-network-memory.py
```

The script runs control-frame and incomplete-fragment WSS profiles, each without and with `-race`, one at a time: 64 sessions, 256 application streams, real two-minute retention, random 1–3 second padding and simulated weak networking. It preserves existing applications. Each run includes ten seconds of observation before and after; one-second samples retain raw `netstat -mmm`, per-pool failure counters and occupancy/capacity, plus test PID/RSS. Resource checkpoints and independent duplex verification are saved under `artifacts/network-memory-serial/`. Use `--output` and `--cooldown` to change the destination and observation period; the four runs take about eleven minutes.

Compare counter deltas and live pool occupancy alongside released Go heap, goroutines, descriptors, sockets, replay buffers and transport timers. Free kernel pool capacity, short-lived TCP TIME_WAIT and race-instrumentation RSS are separate measurements. A valid netstat report with an omitted zero-denials line is recorded as zero; unsupported, failed or unrecognized readings remain unavailable.


### Parallel stress processes and mbuf events

```sh
GOMAXPROCS=4 go test -c -o artifacts/network-memory-parallel-plain.test ./internal/tunnel
GOMAXPROCS=4 go test -race -c -o artifacts/network-memory-parallel-race.test ./internal/tunnel
python3 scripts/diagnose-network-memory-parallel.py
```

Two plain stress processes run together, then two race processes run together; build variants run serially. One process sends continuous control frames, the other incomplete encrypted fragments, with a two-second start offset. Each uses 64 sessions, four streams each, real two-minute retention, random padding and the same weak-network profile. Use `--streams 8` for 1,024 combined application streams, `--start-offset 0` for near-simultaneous startup, and a separate `--output` directory for each comparison.

System and per-pool mbuf counters are sampled every 100ms. One-second details include each test PID's RSS, TCP descriptors/states and queue lengths, plus system TCP states associated with observed test listener ports. `SGATE_STRESS_LIVE_REPORT` enables atomic live Go/resource snapshots, updated once per second while detached. A counter change preserves both neighboring samples, fresh process details, live workload phases and `vm_stat`; the first event also captures per-process `vmmap` summaries. These observations have separate timestamps and are not atomic or guaranteed peaks. The observer adds overhead; its memory is excluded from test-process RSS.

Thirty-five seconds of observation follow each build variant. Checks cover expiry, fresh independent duplex traffic, released connections/replay buffers/transport timers, and falling kernel cluster occupancy and test-related TIME_WAIT. Reports default to `artifacts/network-memory-parallel/`. Existing applications and network settings remain in place, and kernel counters remain system-wide measurements.

Run observer warmup/missing-telemetry regressions with `python3 -B -m unittest discover -s scripts -p 'test_*.py'`. Existing summaries preserve variants not rerun, so `--variants race` can continue an unfinished comparison.

For four stress processes per build, run:

```sh
python3 -B scripts/diagnose-network-memory-parallel.py \
  --processes 4 --streams 8 --start-offset 0 \
  --socket-buffer 65536 --churn-interval 4s \
  --reconnect-min 250ms --reconnect-max 10s
```

Each build runs four near-synchronous processes: two control-frame cases and two incomplete-fragment cases, with distinct files and separate data/cleanup validation. The total is 256 WSS sessions and 2,048 application streams, retaining the real two-minute window, random padding and weak-network profile. Plain and race builds still run serially. The default output is `artifacts/network-memory-parallel-4/`; to resume its race variant, retain the same arguments and add `--variants race`. Optional first-event `vmmap` failures/timeouts are recorded without aborting the stress run.

Use `--socket-buffer` to compare test socket buffers (default 16384; `0` keeps OS defaults), without changing system parameters or production configuration. The command above uses 64 KiB buffers and an outage every four seconds to apply greater kernel-pool pressure. Report the 16 KiB variant separately; results from other buffer sizes do not validate it. `--retention-seconds 45` is a shortened diagnostic window only; omit it for the real two-minute test. Optional `--trace` retains probe connection mappings, forwarding counters, and stack/TCP snapshots about twenty seconds into the outage, with additional observation overhead.

The observer waits for every process in a build and retains individual exit codes. A failed build does not skip the other build comparison; failures produce a nonzero final status and `failures.json`, rather than terminating the other cases early or counting failures as passes.

The four-process command explicitly uses production default reconnect backoff (250ms–10s). Use `--reconnect-min 5ms --reconnect-max 25ms` in a separate directory to reproduce the original aggressive retry profile. Stress backend admission and application-close observation use a 20-second budget; notification taking over five seconds additionally saves application endpoints, the TCP table and stacks. Original session-expiry deadlines and cache limits remain strict. Reports separate `unconfirmed_release_milliseconds` from `unconfirmed_application_release_milliseconds`, so proxy release and application-side notification are not conflated.

For a direct TCP shutdown control without TLS/WebSocket/tunneling, run `SGATE_STRESS_SOCKET_CLOSE=1 GOMAXPROCS=4 go test ./internal/tunnel -run "^TestStressDirectTCPShutdown$" -count=1 -v`, then repeat with `-race`. It establishes 1,024 connection pairs before filling unread queues, closes the opposite endpoints, and requires every peer to observe EOF/reset within five seconds. Configure connection count and buffers with `SGATE_STRESS_SOCKET_CLOSE_COUNT` and `SGATE_STRESS_SOCKET_CLOSE_BUFFER`. This control creates substantial kernel queue pressure; run it separately from the tunnel comparison.

For eight stress processes per build:

```sh
python3 -B scripts/diagnose-network-memory-parallel.py \
  --processes 8 --streams 8 --start-offset 0 \
  --socket-buffer 65536 --churn-interval 4s \
  --reconnect-min 250ms --reconnect-max 10s \
  --output artifacts/network-memory-parallel-8-defaults
```

Each build has 512 WSS sessions and 4,096 concurrent application streams: four continuous Ping/Pong cases and four incomplete encrypted-fragment cases. It retains the real two-minute window, random padding and weak-network profile. Builds run serially with 35 seconds of observation after each; every case keeps independent integrity checks, cleanup assertions and exit codes. The eight-process default directory is `artifacts/network-memory-parallel-8/`, separate from the four-process records. Optional `vmmap` inspection now runs in parallel background workers with actual start/completion timestamps, avoiding eight serial inspections blocking mbuf sampling. Interpret global counters and event details against their actual observation timestamps.

Unconfirmed probe handshake failures now retry with the run's reconnect backoff, sharing the old session's original deadline. Missing session coverage by expiry still fails. An early disconnect after successful upgrade still fails and is not hidden by retrying. Probe control writes, raw fragments and Ping replies use the configured transport write budget, avoiding Gorilla's default one-second reply budget under race/weak-network load. `unconfirmed_upgrade_retries` records handshake retries and `unconfirmed_probe_write_timeout` records the effective write budget. Counters are saved before coverage assertions so failed reports retain them too. Production timeouts, protocol and session retention remain unchanged.

For sixteen stress processes per build:

```sh
python3 -B scripts/diagnose-network-memory-parallel.py \
  --processes 16 --streams 8 --start-offset 0 \
  --socket-buffer 65536 --churn-interval 4s \
  --reconnect-min 250ms --reconnect-max 10s \
  --output artifacts/network-memory-parallel-16-defaults
```

Each build has 1,024 WSS sessions and 8,192 application streams: eight control-frame probes and eight incomplete encrypted-fragment probes. Builds run serially, each retaining sixteen logs, reports, exit codes and 35 seconds of post-load observations. The default directory is `artifacts/network-memory-parallel-16/`. TCP counts aggregate descriptors owned by the test processes, including both local endpoints and relays; memory aggregates sampled process RSS. System-wide cumulative mbuf denial counts need to be interpreted alongside event details, actual pool occupancy and post-load changes.

Sixteen local processes with eight streams per session can hit ephemeral-port capacity first, producing `can't assign requested address` or connection timeouts. Such a run does not pass integrity validation. Preserve its directory, then use `--streams 4 --output artifacts/network-memory-parallel-16-capacity-control` for a separate capacity comparison, retaining sixteen processes, 1,024 sessions and all other parameters, with 4,096 application streams. Report both load levels separately; a successful capacity control is not an eight-stream pass.

`--clients` configures sessions per process, retaining the default of 64. To further isolate session scale, use `--clients 32 --streams 8 --output artifacts/network-memory-parallel-16-512sessions` for 512 sessions and 4,096 application streams, keeping the other command arguments above. Different client counts cannot share an output directory. Release-time validation failures report the observed milliseconds and permitted range.

Unconfirmed resumes have an independent original-deadline timer that closes the transport, preventing Ping replies waiting on the WebSocket write lock from outliving read deadlines and delaying old-session release. Authenticated confirmation and expiry compete through an atomic state: successful confirmation stops the timer, while expiry rejects confirmation. Transport exit also stops it. A blocked-write regression covers the failure of read deadlines alone; a confirmed-resume regression verifies healthy traffic beyond the original deadline.

For the full local scale of sixteen processes, 64 sessions each and eight streams per session:

```sh
python3 -B scripts/diagnose-network-memory-parallel.py \
  --processes 16 --clients 64 --streams 8 --start-offset 0 \
  --backend-host ::1 --server-host ::1 --cohort-settle 35s --synchronize-load --tunnel-log \
  --socket-buffer 16384 --churn-interval 4s \
  --reconnect-min 250ms --reconnect-max 10s \
  --output artifacts/network-memory-parallel-16-fullscale/final
```

Each cohort retains 1,024 WSS sessions and 8,192 simultaneously active application streams. The test HTTP server and backend use IPv6 loopback; clients and relay ingress remain IPv4. A 35-second wait between completed application cohorts allows kernel TIME_WAIT entries to drain. It never occurs inside live load, disconnect injections, or the original two-minute retention window. Reports retain `backend_host`, `server_host` and `cohort_settle`, and reject mixed configurations in one directory. The observer records temporary-port occupancy and states for both loopback families with the counted range, and saves TCP/VM details at the first failed child. Backend dial failures log their actual cause, excluding normal session cancellation.

Offered-load duration starts after all backend streams are ready, so slow admission cannot consume traffic time or disconnect injections. `load_windows` records each round's readiness, send deadline and full stream count. With near-simultaneous startup, the observer requires every process's full-load window to share an intersection in each round, recorded as `full_load_overlap_seconds`. Between-cohort waits cannot substitute for full concurrency.

The full-scale command uses the fixture's default 16-KiB socket buffers. Bounds cover application endpoints, accepted proxy endpoints and the proxy's outbound backend sockets, set after every stream entry is ready and before payloads start. `backend_sockets_bound` checks coverage across both normal cohorts and the expiring cohort. A separate 64-KiB capacity control with 8,192 unread duplex streams can exhaust local mbufs before either address family's ephemeral ports fill, producing `socket: no buffer space available`. Record this separately from address-allocation failures; it does not by itself establish a Go heap leak, and a failed profile is not an integrity pass.

Transport Ping handling queues a bounded pending Pong for the existing writer, rather than waiting for the WebSocket write lock in the reader. This keeps ACK processing moving during outbound backpressure. Only the latest pending payload is retained, as permitted by [RFC 6455 §5.5.3](https://www.rfc-editor.org/rfc/rfc6455.html#section-5.5.3), outside the reliable replay queue. A regression blocks a data write, sends Ping and an authenticated ACK, requires ACK/cache progress while the write remains blocked, then verifies the echoed Pong after write progress resumes.

The weak-network fixture coalesces only adjacent segments whose individual send times have arrived, never passing a future segment or coalescing in bandwidth-spacing mode. It skips already-expired timer waits and reports `relay_coalesced_packets`, `relay_writes` and `relay_wait_timers`. Baseline collection waits for both peers and all transport writers. Expiring-stream verification still checks every delivered byte without generating an unread expected tail.

Run `python3 -B scripts/diagnose-port-capacity.py --output artifacts/tcp-port-capacity.json` separately for a pure kernel port control. It uses no sgate or application payload: hold IPv4 TCP connections, then probe a new IPv4 target and IPv6, saving raw before/after mbuf tables. It raises only its own process's descriptor soft limit and never changes host port ranges or network-memory settings.

For a memory-capacity control at the same 16 processes × 64 sessions × 8 streams, change the full-scale command to `--socket-buffer 8192` and use a separate `artifacts/network-memory-parallel-16-fullscale/bounded-8k` directory. This bounds test endpoints without changing production settings, session queues, or concurrency. Unbounded transport endpoints still use OS buffers, so this does not guarantee relief from mbuf exhaustion. Report 16 KiB and 8 KiB profiles separately.

Transport errors retain both the original writer failure and the reader failure caused by socket shutdown. Failed unconfirmed probes include the relay’s first close reason, direction, and byte counters without enabling packet-by-packet logs. Failed stress runs also sample `closed` resources after cleanup; their exit status remains failed, and successful cleanup does not establish data integrity.

The opt-in `TestConfigBinaryNoiseReconnect` accepts `SGATE_CONFIG_CLIENT_BINARY` and `SGATE_CONFIG_SERVER_BINARY` paths pointing to matching ordinary or race builds. It verifies actual configuration-file noise settings and CLI overrides/disables across WS/WSS, including 1–3 second waits and 1–10 KiB payloads, with three reconnects and four original application streams per case. A local observer decrypts only disposable test traffic and forwards ciphertext unchanged; both WSS legs validate certificates. Control frames are handled on each relay leg, so this test does not establish end-to-end heartbeat timing. Internal pending queues and hour-long timer cancellation are checked separately by `TestNoiseDirectionTimersReleaseOnExpiry` and `TestNoisePendingTimerReleasedOnExpiryAndShutdown`.

`TestConfigBinaryNoiseLifecycle` uses the same binary environment variables with `-parallel=6 -timeout=6m`. Its 18 cases cover WS/WSS, file-enabled noise and CLI modifications/disables, and long outage, server process restart, or session expiry. Long outages use real two-minute retention and block recovery for 125 seconds; a separate 750ms server/30s client expiry profile accelerates only the expiry cases. Old streams offer 2 MiB per direction during the outage; fresh streams use independent record/EOF verification and per-session cumulative ACK checks. Expiry/restart aborts the old streams by design, and buffered old bytes must not contaminate the new streams. The observer preserves authenticated HTTP 410 responses. `TestConfigNoiseLifecycleResources` separately checks internal maps, queues and writer/timer completion, including hour-long pending timers.

`TestConfigBinaryNoiseWeakNetwork` uses the same binary variables with `-parallel=6 -timeout=4m`. Six cases cover WS/WSS and file-enabled noise (1–3s, 1–10 KiB), CLI modifications (100–300ms, 128–512 bytes), or disabling both ends. A transparent TCP relay forwards TLS and all WebSocket control frames without answering Ping/Pong. Each case runs 22 seconds of 150±100ms one-way latency with 15% delayed segment retransmissions (200ms extra), exceeding the 18-second heartbeat timeout; stalls each direction for one second without reconnecting; then blocks each direction until automatic reconnect and restores it. Two original application streams verify independent bidirectional records, terminal markers and EOF without opening replacement backend connections. Loss is modeled as ordered TCP retransmission delay, rather than actual kernel packet drops.

`TestConfigNoiseWeakNetworkResources` checks matching effective option states using private stream/replay ownership and transport writer completion, including cancellation of hour-long pending timers after one-way expiry. It complements parser-priority and actual binary/wire tests; it does not parse configuration files itself. The relay's `waitTimers` counts cumulative timer creations, not live timers. Cleanup relies on joining all relay goroutines, empty queues and completed transport writers.

The opt-in `TestConfigBinaryNoiseSoak` uses disposable diagnostic builds of the original command entry points. Build them with `python3 -B scripts/build-config-soak.py` and repeat with `--race`. Go overlays inject private resource snapshots and runtime sampling without editing production sources; generated sources use `.overlay` extensions so `go test ./...` does not discover them as packages. Diagnostic binaries are for testing only.

```sh
SGATE_CONFIG_SOAK=1 \
SGATE_CONFIG_CLIENT_BINARY="$PWD/artifacts/config-noise-soak/sgate-client" \
SGATE_CONFIG_SERVER_BINARY="$PWD/artifacts/config-noise-soak/sgate-server" \
SGATE_CONFIG_SOAK_REPORT_DIR="$PWD/artifacts/config-noise-soak/plain" \
GOMAXPROCS=4 go test ./internal/tunnel -run '^TestConfigBinaryNoiseSoak$' \
  -parallel=6 -count=1 -timeout=8m -v
```

Run the race comparison separately with both binary paths ending in `-race`, a distinct report directory, and `go test -race`. Six parallel WS/WSS × file/CLI override/CLI disable cases each hold 128 application streams across four 75-second load cohorts (300 seconds of load per case). A transparent TCP relay injects 150±100ms one-way latency, 15% delayed retransmissions with 200ms extra delay, and closes the tunnel every six seconds. Production reconnect backoff and two-minute retention remain configured. Each stream independently verifies records and EOF in both directions; later cohorts use different seeds. Noise is configured at 1–3s/1–10KiB, overridden to 100–300ms/128–512 bytes, or disabled by explicitly clearing both interval bounds.

Every two seconds, diagnostics sample heap, RSS, goroutines, private stream/replay/nonces, and transport writer ownership. Each completed cohort must have empty stream/replay state and one writer per peer; after GC, heap growth above the first drained cohort is limited to 8MiB, goroutines to baseline+12, descriptors to baseline+4, and server nonces to 64. The report retains all samples for trend analysis rather than treating these ceilings as proof of no leak. File descriptors use a macOS `lsof` fallback during quiescent samples. Final shutdown requires zero sessions, streams, replay bytes, nonces and transport writers; the relay is joined with zero active connections and queued bytes. Writer completion is the timer cleanup oracle, not enumeration of Go runtime timers. Sampling itself allocates a fixed 4MiB stack census buffer and invokes `ps`; compare post-GC samples at equal load. This is a five-minute load profile, not an overnight or kernel packet-drop test. `SGATE_CONFIG_SOAK_SMOKE=1` selects only a short four-stream preflight and must be reported separately.

### Client-selected backend targets

A single server process can route different client sessions to different backends. Keep the required default `target` and configure a comma-separated `allowed-targets` list, for example `127.0.0.1:8088,backend.example.com:443`. Clients select a backend using `server = "wss://gateway.example.com/tunnel?addr=127.0.0.1:8088"`. Without `addr`, the default target is used. An empty allowlist disables explicit selection, including explicit selection of the default target. The CLI `-allowed-targets` overrides the file; saving this setting in the management form persists and applies it immediately without restarting the proxy. Explicit CLI values retain priority. Existing tunnels keep their bound backend; new tunnels and reconnects use the latest allowlist and removed targets receive `403`. Clearing the list disables explicit selection while leaving the default backend available.

Addresses use exact full `host:port` matching; DNS names and bracketed IPv6 are supported, without wildcard/CIDR or resolved-IP alias matching. URL-encode query values when constructing URLs. Empty, duplicate or malformed `addr` values return 400; disallowed targets return 403 after handshake authentication. The authenticated URI includes the query string. Each session remains bound to its first backend; a resume selecting a different backend returns 409 without changing the original target or retention deadline. Restart the client proxy with a different URL to establish a new session for a different backend. Applying server configuration closes old proxy sessions.

Client backend selection can also be configured independently: `server = "wss://gateway.example.com/tunnel"` and `addr = backend.example.com:443`, or CLI `-addr backend.example.com:443`. A nonempty standalone `addr` overrides the URL query selection and is URL-encoded before handshake authentication. Empty standalone `addr` preserves a legacy URL selection; both must be absent to use the server default. CLI values override the corresponding file options. Invalid addresses and empty/duplicate URL `addr` values are rejected before dialing. The server allowlist remains authoritative. Apply the client configuration to start a fresh session when switching backends. Client examples include the new field; the generic example now points to the server example's port 8081.

Client `send-addr` defaults to `true`, preserving existing selection behavior. Set `send-addr = false` to ignore the independent `addr` and remove all `addr` parameters from the request URL, using the server default backend while retaining other query parameters and the saved addresses. CLI `-send-addr=false` or `-send-addr=true` overrides the file. Applying the switch through management replaces the client proxy and closes its old application connections.

The client now uses go-service's `TcpServiceInit → AcceptHandler/CloseHandler → Run` in both standalone and acl_master modes. Management HTTP/HTTPS runs independently; proxy applies preserve its listener and login sessions. Callback lifetime follows the application connection, including TCP half-close. Standalone TCP rebind supports rollback; managed ports require a master_ctl restart. Runtime reports `tcp_framework=go-service` and `tcp_connections`. The local v1.1.2 lifecycle extension lives in third_party/go-service; the reference vendor/go-service/examples/go-echod is unchanged.

Client `proxies` accepts a JSON array of `{ "listen": "127.0.0.1:9001", "server": "wss://gateway:8081/tunnel?addr=127.0.0.1:22" }` mappings. Optional per-entry `addr` overrides that URL's backend. An empty setting preserves single-proxy `master_service/server/addr`; a nonempty list replaces those top-level settings. Up to 64 distinct nonzero local ports share process-wide authentication, TLS trust, heartbeat, reconnect and noise settings. Each mapping owns a separate tunnel/session and its own `max-streams`/`buffer-bytes` limits; bandwidth statistics aggregate the process. The server allowlist still applies, and global `send-addr=false` removes backend selection from every mapping. CLI `-proxies` replaces the full list; `-proxies ''` restores single-proxy mode. Under acl_master, `master_service` must declare exactly the mapping ports. Mapping saves take effect online. Standalone changes bind additions before committing and preserve connections on unchanged mappings; removed or retargeted mappings close their own streams. Managed port changes automatically restart the entire client through the deployed stop/start scripts, interrupting all its application connections while preserving management login sessions.

Client `master_service` must include only proxy TCP ports, never `admin_listen`; the independent HTTP management goroutine binds that port. Wildcard proxy ports may expand into multiple interface listeners under acl_master and share the same proxy mapping.

The client and server consoles support Simplified Chinese and English. Select a language on the login page or dashboard; switching is immediate and preserves login sessions, unsaved edits and password inputs without submitting configuration. The first visit follows the browser language (English or otherwise Chinese); subsequent visits use the saved browser preference. Configuration keys, addresses and custom comments are unchanged. Deploy the entire updated `web/admin/` directory, including `i18n.js`.

Saving client mappings in the management UI now synchronizes the existing `master_service` entry in the same configuration transaction. Clearing mappings uses the effective single-proxy `listen`; command-line overrides remain authoritative. Validation or apply failure rolls both settings back. Row saves refresh the linked listener value while preserving other drafts. For managed port additions/removals, saving returns service_restart_required; the UI automatically requests the whole-service restart, waits for the old process to exit and new sockets to open, then confirms a different running PID. Failures retain a pending-restart indicator. Same-port backend edits update only affected mappings online. Manual `.cf` edits still require both settings to be kept consistent.

### Port mapping failure logs

Client logs identify a mapping by `listen`, `server`, and `target`, and an application stream by `stream`. Tunnel connection logs include `session` for correlation with server logs. When backend dialing fails, the server logs the actual target and OS error; an encrypted control message reports refusal, timeout, DNS failure, unreachable network/host, stream limit, or a generic failure to the client. Only the affected stream closes. Error messages never enter application payloads.

Allowlist rejection, authentication/handshake failures, session conflicts, and capacity limits are also logged. Passwords, authentication headers, and unrelated URL query parameters are omitted. Legacy clients receive compatible empty resets; when an older server supplies no reason, newer clients suggest checking the peer log. Upgrade both programs for detailed backend failure categories on the client. If a request never reaches the server, such as an unreachable tunnel port or a TLS failure, consult client and framework/TLS logs.

### Mapping connectivity tests

Each client mapping has a **Test** button. It probes the row's draft addresses using the client's effective password, key, TLS, and `send-addr` settings, and displays the result, stage, reason, and elapsed milliseconds. With `send-addr=false`, it tests the server's default backend. A dedicated short-lived WS/WSS connection carries encrypted `TEST_CONNECT` and `TEST_CONNECT_RESULT` commands. The server dials the selected backend only after validating the command; the page and both logs share a `test_id` for correlation. Failed tests show the target, error category, and concrete system error, also recorded by the client; the server logs the original backend error and returns encrypted command rejection details. Timeout, cancellation, and early disconnect have distinct messages. The test verifies command exchange and one backend TCP connection; it does not save settings, bind the local mapping port, restart the proxy, send application data, or check application protocols. No tunnel session is retained. Both programs must be upgraded; older servers report that an upgrade is required. Tests require administrator authentication and CSRF validation, have timeouts, and are limited to four concurrent tests on each side.


The live traffic chart supports filtering total, business payload and noise payload by tunnel peer IP. Clients group by the remote server's TCP IP; servers group by the incoming peer IP. Connections from the same IP are combined; HTTP forwarding headers and backend addresses are not used. The filter affects the chart and its IP summary, while top cards retain process totals. Closed peers remain for one minute and are removed on subsequent sampling. Up to 1024 IPs are tracked individually; additional peers share an Other IPs bucket. Per-IP counters may restart after eviction, so rates require two valid consecutive samples.


### Multiple tunnel accounts

Set server `accounts-file = tunnel-accounts.json` to enable independent client identities. Relative paths resolve from the service `.cf` directory; `-accounts-file` overrides the file value. At startup, a missing configured account file is created as a version-1 document with an empty accounts array. New directories use mode 0700 and the file uses 0600. The server starts normally, and administrators can add accounts through Client accounts without restarting. The empty list rejects every tunnel and never falls back to the common password. Existing files, including invalid documents, are never overwritten. Leaving accounts-file empty retains legacy authentication. The local JSON schema is:

```json
{"version":1,"accounts":[{"username":"alice","password":"replace-alice-password"},{"username":"bob","password":"replace-bob-password","disabled":false}]}
```

Clients can enter credentials in Proxy configuration → Server connection authentication and click Save and apply. Both inputs are available for older configurations missing these keys; saving adds them to the configuration. Unedited passwords are preserved. Alternatively, set `username` and `password` in the `.cf`, or use `-username` and `-password` (CLI values take priority). Mapping entries without independent credentials inherit these settings. Usernames are case-sensitive, contain 1–128 UTF-8 bytes without whitespace or control characters, and passwords contain 1–1024 bytes. Multiple clients may use one account. Usernames and passwords travel together in a request-bound encrypted header using the existing shared `sgate.key`. Sessions are bound to the creating account; another account cannot resume them.

A nonempty `accounts-file` disables legacy password-only authentication. An empty setting preserves the legacy behavior; legacy clients leave `username` empty. Tunnel accounts are separate from Web administrator users. Existing global backend allowlists and session limits still apply. The server-only Client accounts page supports adding users, changing passwords, enabling/disabling and deleting accounts. Saves atomically persist JSON and apply online without restarting the proxy. Password changes, disabling or deletion revoke only that account's existing sessions; other accounts continue. Blank password edits preserve the credential, and list responses never expose passwords. After external JSON edits, refresh the list and click Apply account file; proxy/process restart also reloads it. Saving a changed `accounts-file` loads the selected JSON online without restarting; saving unrelated settings does not reload external JSON. An empty accounts array rejects every account without falling back to the legacy password. Set `disabled: true` to disable an account. Unreadable or invalid files fail closed, and a failed apply retains the running instance.

Protect the plaintext-password JSON like the legacy `.cf` (for example mode 0600), outside the Web asset directory and source control. The file is limited to 1 MiB and 4096 accounts; duplicate usernames and unknown fields are rejected. See `samples/sgate-accounts.json`; sample accounts are disabled. Full/server installation includes the sample without enabling it or overwriting existing account files.

Changing and saving `accounts-file` loads it online, with a Takes effect immediately on save badge. Failed validation/loading retains the original configuration and authentication policy. Clearing it restores the already-active common password, and is rejected if no valid password is active. Mode changes revoke old-mode tunnels; switching files preserves sessions with unchanged credentials. Account management uses the effective CLI-selected accounts-file, requires authenticated same-origin CSRF-protected requests, rejects stale file revisions and restores the file on apply failure. For first use, enter credentials in Client accounts and click Add and enable multiple accounts. The server exclusively creates tunnel-accounts.json beside the .cf, persists accounts-file and enables authentication online without restarting the proxy. Legacy password-only tunnels disconnect on activation. Existing files are never overwritten; select and save an existing file in accounts-file to load it online. An explicit -accounts-file= CLI override remains authoritative and must be removed before UI initialization.


### Account-based reverse TCP proxy

Add optional `reverse_mappings` to each server-side account in `tunnel-accounts.json`:

```json
{
  "version": 1,
  "accounts": [{
    "username": "alice",
    "password": "replace-with-a-unique-password",
    "reverse_mappings": [
      {"listen": "127.0.0.1:10022", "target": "127.0.0.1:22"},
      {"listen": "0.0.0.0:18080", "target": "192.168.1.20:8080"}
    ]
  }]
}
```

`listen` binds on the **server** and requires an explicit literal IP and numeric port 1–65535; IPv6 uses `[::1]:10022`. `target` is dialed from the **client's network** and accepts a loopback address, another IP, or a hostname with a numeric port. Duplicate listen addresses are rejected. Limits are 256 mappings per account, 4096 per server, and the existing 1 MiB account document limit.

In server **Client accounts**, use **Reverse proxy port mappings** under each account to add, edit or remove rows, then click that account’s **Save** to apply them online. New accounts can include mappings during creation. Removing every row and saving clears the account’s mappings. The form validates listen IPs, port ranges, duplicate addresses and the 256-mapping limit. Failed saves retain drafts; saving another account preserves unsaved edits.

Set server `accounts-file = tunnel-accounts.json`, configure the client with the same key, server URL, username and password, explicitly opt in on the intended client mapping, and start both normally. The client establishes its WebSocket proactively; no incoming forward TCP connection is needed. Forward and reverse proxying can coexist. Upgrade both endpoints for reverse support. Legacy anonymous/password-only clients and clients without the authenticated reverse capability are never selected. If an account has multiple confirmed online sessions, the server chooses the first in session-ID order. Assign different accounts to different devices to make target routing unambiguous.

Each row in client **Proxy configuration → Multiple port proxy mappings** has an independent **Allow server reverse proxy** checkbox, disabled by default. Older mappings without the field also deny reverse access. Saving a checked row allows only that row’s tunnel to advertise and receive reverse commands; other rows using the same account do not inherit permission. Set the Boolean `allow_reverse` in external or inline mappings:

```json
[
  {"listen":"127.0.0.1:9001","server":"wss://example.com/tunnel","username":"alice","password":"replace-password","allow_reverse":true},
  {"listen":"127.0.0.1:9002","server":"wss://example.com/tunnel","username":"alice","password":"replace-password","allow_reverse":false}
]
```

Revoking and saving replaces only that mapping’s worker and closes its existing forward and reverse streams. An online client sends a transport-bound authenticated close notification to release retained server sockets; ordinary network failures retain resumability. URL query parameters cannot enable consent, and disabled clients reject forced reverse commands without dialing targets. Legacy single-proxy mode has an explicit `allow-reverse = true` / `-allow-reverse=true` switch, default false; it never grants consent to JSON mapping rows. Upgrade both endpoints for these controls.

Saving account mappings synchronizes `tunnel-accounts.json` and `master_service` in the captured server service `.cf`, preserving HTTP listeners and removing deleted or disabled reverse listeners. In managed mode, listener changes automatically run the server stop script, wait for the old process to exit, then run the start script. All business connections are interrupted. The page verifies a new running PID and every expected listener before reporting success, retaining other drafts. Target/password changes with unchanged listeners remain live. Framework sockets reserved for reverse mappings are leased as raw TCP and never served as HTTP/TLS. Preflight or live-apply failure rolls back both files; restart failures retain saved configuration and show an error for retry.

After external JSON edits, refresh Client accounts and click Apply account file to synchronize listeners and perform any required service restart. For manual deployment, also update `master_service`, then run `sgate-server-stop.sh` followed by `sgate-server-start.sh`. Account create/update API operations also accept `reverse_mappings`; omission preserves existing mappings, and `[]` clears them. Mapping changes revoke that account's tunnels and business connections, allowing automatic reconnection under the new configuration; other accounts remain connected. New binds must succeed before policy changes commit. Disabled/deleted accounts and switching to legacy authentication release their listeners. Shared HTTP proxy replacement transfers matching listeners. Offline accounts reject new TCP connections immediately.

Reverse commands and data use the existing authenticated encrypted channel, separate stream-ID ranges, bounded buffers, stream limits, heartbeats, half-close/reset and reconnect replay. Existing business sockets survive brief transport outages within the configured retention window. Target dial failures reset only their stream. Grant reverse permission only to trusted servers: opted-in clients follow their reverse target commands. Wildcard binds expose the mapped service on network interfaces; use explicit interfaces, firewalls and service authentication as appropriate. External-to-server and client-to-target TCP segments rely on the target service's own security protocol. WSS is recommended. See `samples/sgate-reverse-accounts.json`, whose sample account is disabled by default.


### Reverse proxy stress and boundary tests

The normal reverse suite uses real TCP and WS/WSS connections: 64 concurrent streams across four waves, byte-exact echoes and half-close; eight retained streams across four outages with full replay buffers; shutdown while the target stops reading; repeated account disable/enable under concurrent admissions while another account keeps transferring; sender/receiver stream limits, exact buffer capacity and ACK recovery, final stream ID, 256/4096 mapping limits, expired-session rejection, and malformed commands. Stress fixtures check goroutine/file-descriptor recovery and session, stream and queue release.

```bash
GOPROXY=off go test -mod=mod -race ./internal/tunnel -run '^TestReverse' -count=3 -timeout=3m
SGATE_REVERSE_STRESS=1 GOPROXY=off go test -mod=mod -race ./internal/tunnel -run '^TestReverse' -count=1 -timeout=5m -v
GOPROXY=off go test -mod=mod ./internal/tunnel -run '^$' -fuzz '^FuzzReverseOpenFrame$' -fuzztime=30s -parallel=2
```

Extended mode runs 128 concurrent streams across eight waves at 256 KiB per stream, 32 retained streams across 12 outages, and 48 account disable/enable cycles for both WS and WSS. It changes test load only. Mapping-count boundaries validate configuration without binding thousands of ports. These commands select local replacements from `go.mod` and prohibit dependency downloads; dependencies must already be cached.


### External mapping files and per-mapping credentials

The default client now stores its original mapping in `sgate-proxies.json`, preserving its addresses and inherited credentials. Installation puts this file in `conf/` and preserves existing copies on reinstall. Inline mappings remain supported. Set `proxies-file = 'sgate-proxies.json'` in the client `.cf`, or use `-proxies-file /absolute/path/routes.json`. Relative paths resolve from the `.cf` directory. The JSON file is an array:

```json
[
  {"listen":"127.0.0.1:9001","server":"wss://example.com/tunnel","addr":"127.0.0.1:22","username":"alice","password":"alice-password"},
  {"listen":"127.0.0.1:9002","server":"wss://example.com/tunnel","addr":"127.0.0.1:8080","username":"bob","password":"bob-password"}
]
```

A nonempty file option overrides inline `proxies`. An explicit CLI `-proxies` (including empty) overrides a file option from configuration; an explicitly supplied `-proxies-file` wins if both are supplied. Each mapping's username/password overrides the corresponding common credential; omitted fields inherit the common setting, including its CLI override. An explicit empty username selects legacy authentication; an explicit empty password is invalid. Key, TLS, heartbeat and noise settings remain shared.

An empty array uses the single-proxy settings. On startup, a missing file is created from inline mappings, or as `[]` to use single-proxy settings. Relative paths still resolve from the `.cf` directory. New directories use mode 0700 and the file uses 0600; existing files are never overwritten. Permission errors or invalid files still fail with an error. Validation and online loading do not automatically create files. Limits are 1 MiB and 64 mappings. The UI supports independent authentication per mapping and never returns saved passwords. Blank edits preserve an existing independent password; otherwise they inherit the common password. Disable independent authentication to inherit both common credentials. Changing a mapping's listen address requires re-entering its independent password.

Changing `proxies-file` in the UI migrates mappings to a new file or loads an existing file; clearing it migrates mappings back inline. Saves synchronize `master_service`, apply mapping and credential changes online, preserve unchanged mappings, and roll back both files on failure. Managed listener-port changes still restart through `master_ctl`. External JSON edits require reloading and saving/applying; files are not watched automatically.

Protect JSON passwords with mode 0600 and keep the file outside Web assets and source control. UI-created files and backups use 0600. Installation includes the inactive `samples/sgate-proxies.json` example without overwriting existing files.

Managed client listener changes use the deployed `bin/sgate-client-stop.sh`, wait for the old process to exit, then run `bin/sgate-client-start.sh`. Source deployments use scripts beside the service configuration. Both scripts are checked before stopping. The Web flow passes the captured framework configuration through `SGATE_CONFIG`, preserves `MASTER_CTL` and `MASTER_SOCKET`, and sets `MASTER_CTL_TIMEOUT=30000` (milliseconds). A failed stop prevents start. The UI verifies a running replacement PID and matching listeners before reporting success. Install the updated scripts together with the binary.

acl_master identifies services by the literal `-f` path. Web restarts retain path spelling, including `bin/../conf`, `./` and symlinks. The client submits asynchronously; a detached controller waits for the stop script, old-process exit and start script in order. The browser waits for the replacement PID and listeners.

Managed client/server services send START_OK over FD 5 when Run begins after successful initialization. This is separate from busy/idle status notifications. master_ctl -t uses milliseconds; client scripts and Web restarts use 30000 (30 seconds). Failed initialization never acknowledges readiness. scripts/test-start-ready.py validates real framework startup replies, repeated WS/WSS launches, 128 streams and port cleanup.


Each reverse mapping now supports independent `listen_tls` and `target_tls` booleans, both false by default. The server UI offers SSL/TLS toggles next to both addresses. An SSL listener reuses the loaded tunnel certificate unless both `listen_tls_cert` and `listen_tls_key` specify separate server-side PEM files. Relative paths use the server working directory; absolute paths are recommended. Missing or invalid certificates reject the save, including managed restart staging. Target TLS uses the authenticated `REVERSE_OPEN_TLS` opcode (7), only for clients advertising `sgate-reverse-tls=1`; unsupported peers are never downgraded to plaintext. Clients verify the target hostname/IP and certificate chain, using system roots plus their `tls-ca` when configured. TLS requires 1.2 or later. Upgrade both programs. Entry TLS and target TLS terminate separately, while the existing AEAD WebSocket tunnel continues carrying application bytes. See the [architecture document](../docs/architecture.md#81-反向映射两端的-ssltls).

QUIC noise now interprets `noise-min-bytes` / `noise-max-bytes` as per-tunnel payload bytes per second, paced in small frames without catching up after pauses or congestion. Interval settings control target-rate reselection (minimum 1s); all-zero intervals disable noise. WS/WSS retains bytes-per-burst semantics. Update and restart both endpoints to apply the change; protocol overhead is additional to the payload rate.
