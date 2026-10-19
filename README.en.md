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
./bin/sgate-server -listen :8080 -target 127.0.0.1:5432 -key-file sgate.key
```

On the client:

```sh
./bin/sgate-client -listen 127.0.0.1:15432 \
  -server ws://SERVER_IP:8080/tunnel -key-file sgate.key
```

Connect your application to `127.0.0.1:15432`. Alternatively, supply the 64-character hexadecimal key through `SGATE_KEY`.

For public deployments use `wss://`. The server supports `-tls-cert` and `-tls-key`; the client verifies certificates and supports additional trust roots with `-tls-ca`. A WebSocket-capable HTTPS reverse proxy can terminate TLS instead.

## Recovery

The client reconnects automatically with exponential backoff and jitter. Both endpoints retain TCP sockets and unacknowledged frames across temporary WebSocket outages. Sequence numbers, cumulative acknowledgements, and deduplication preserve byte order without repeating delivery. TCP half-close is supported.

The default disconnected-session retention is two minutes (`-session-ttl`). Restarting either process, session expiry, backend disconnects, or application timeouts cannot preserve existing application sockets. On session loss, old application connections close and the client starts a new session for subsequent connections. An ACK confirms forwarding to a TCP socket, not completion of a business transaction.

Each direction buffers at most 4 MiB of unacknowledged frames by default (`-buffer-bytes`), then applies TCP backpressure. The default limits are 128 streams per session (`-max-streams`) and 128 retained sessions on the server (`-max-sessions`). Socket buffers and per-stream read buffers consume additional memory. Slow backend writes can temporarily delay other streams in the same session.

Transport messages and controls are encrypted even with `ws://`; HTTP upgrade headers remain visible without TLS. Standard HTTP/WebSocket compatibility does not guarantee resistance to traffic classification. The ordinary HTTP root returns a small page.

## Verification

```sh
go test -race ./...
go vet ./...
```

Tests cover tampering, replay, authentication failures, multiplexing, half-close, reconnect with a 2 MiB payload on the original backend socket, lost acknowledgements, bounded buffering, expiration, and backend failure.
