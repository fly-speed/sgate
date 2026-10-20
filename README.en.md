# sgate

A Go TCP tunnel with two executables. `sgate-client` accepts local TCP connections and multiplexes them over one persistent, authenticated, AES-256-GCM encrypted WebSocket connection. `sgate-server` opens one TCP connection per stream to a fixed backend. Traffic flows in both directions.

See [the Chinese documentation](README.md) for all options, protocol details, and reverse proxy configuration.

## Build

Requires Go 1.23 or newer.

```sh
make build
make test
```

## Run

Generate a random shared key and securely copy the same key file to both endpoints:

```sh
umask 077
openssl rand -hex 32 > sgate.key
```

On the server:

```sh
./bin/sgate-server -listen :8080 -target 127.0.0.1:5432 -key-file sgate.key -password 'your-password'
```

On the client:

```sh
./bin/sgate-client -listen 127.0.0.1:15432 \
  -server ws://SERVER_IP:8080/tunnel -key-file sgate.key -password 'your-password'
```

Connect your application to `127.0.0.1:15432`. Alternatively, supply the 64-character hexadecimal key through `SGATE_KEY`.

Both endpoints require the same `-password` value (1–1024 bytes). Replace `your-password` in the examples. The password is separate from `sgate.key`.

The client includes an `X-Sgate-Password` header in every HTTP/HTTPS WebSocket handshake, including reconnects. It contains a versioned Base64URL encoding of a fresh nonce and AES-256-GCM ciphertext, using a dedicated encryption key derived from the shared key. Authentication binds the encrypted credential to the session, request nonce, timestamp, resume flag and URI. Request and response proofs also bind the credential.

The server decrypts and verifies the password before upgrading or allocating a proxy session. Missing, incorrect, tampered or duplicated credentials return HTTP 404 without creating a proxy session or backend connection. Replayed handshakes are also rejected within the running server's in-memory replay window. Passwords are required at startup and are never sent as plaintext headers. The ordinary HTTP homepage remains public. Upgrade both programs together: older key-only handshakes are no longer accepted.

For public deployments use `wss://`. The server supports `-tls-cert` and `-tls-key`; the client verifies certificates and supports additional trust roots with `-tls-ca`. A WebSocket-capable HTTPS reverse proxy can terminate TLS instead.

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

## Resource stress test

```sh
make stress
SGATE_STRESS_DURATION=3m GOMAXPROCS=4 make stress
SGATE_STRESS=1 GOMAXPROCS=4 go test -race \
  -run '^TestStressReconnectResources$' -v -timeout 10m ./internal/tunnel
```

The default workload uses four clients and 64 TCP streams per client, cycling through 12 rounds with 128 KiB exchanges and an injected outage every 30 ms. Each outage blocks reconnects for 10 ms. When a duration is configured, each batch keeps its sockets open for repeated exchanges before half-closing and cycling connections.

`SGATE_STRESS_CLIENTS`, `SGATE_STRESS_STREAMS`, `SGATE_STRESS_ROUNDS`, `SGATE_STRESS_BYTES`, and `SGATE_STRESS_DURATION` control the workload. `make stress` saves JSON samples to `artifacts/stress.json`; override with `SGATE_STRESS_REPORT`. Direct `go test` invocation only saves a report when that variable is set.

Samples include heap, stack, goroutines, file descriptors, accepted TCP connections, retained sessions, replay buffers, and authentication nonce counts. Idle checkpoints force GC and also record RSS (`ps`); macOS descriptor counting uses `lsof`. Runtime pages and race-detector shadow memory can remain resident, so assess RSS trends together with retained heap and resource counts. Nonces expire within a two-minute retention window. Assertions check idle recovery and shutdown cleanup. Concurrent error reports for the same stream produce only one RESET, preventing duplicate notifications from exhausting control work and closing unrelated streams.

## Weak-network tests

Run `make weaknet` for three race-enabled repetitions. A bounded local TCP relay models 150ms one-way delay, ±100ms jitter, 15% delayed segment retransmissions at 200ms, slow payload delivery and short/long blackholes in each direction, including TLS transport coverage. It preserves ordered TCP delivery: synthetic losses delay retransmission rather than remove bytes from a TCP stream. It does not modify NIC or kernel packet-loss rules. Size the receive timeout to include complete TLS record delivery on TLS links.

Checks cover idle heartbeat stability, random byte-for-byte exchanges, exact backend byte counts, half-close tails, resume on the original application socket, expiry and fresh connections. Already-delivered bytes can remain in the old application's receive buffer before EOF/reset. Relay connections, queued bytes, application sockets, replay queues and goroutines must all be released during cleanup.
