# gin-service lifecycle extension for sgate

Based on acl-dev/gin-service v1.0.2, with its LICENSE and upstream documentation
retained in UPSTREAM.md. The supplied reference repository under
`vendor/gin-service` is unchanged. This module is the implementation selected by
the root go.mod replacement; scripts/sync-go-vendor.py copies it into standard
Go vendor layout.

The server uses the example's `Init -> register routes -> Run` sequence in both
standalone and acl_master modes. Init owns binding or inherited FD acquisition,
the state-channel monitor and Gin engines. Run starts HTTP workers and joins
shutdown. It deliberately replaces the upstream process-global Wait/Stop monitor
with per-service context cancellation, so HTTP and hijacked WebSocket shutdown can
be joined safely and proxy reloads do not terminate the parent service.

Additional APIs:

- WithContext and WithManaged select cancellable lifetime and startup mode.
- Configure prepares a Worker before Run, or starts a replacement while Run is
  active. Close the old Worker first; standalone address changes may rebind,
  whereas inherited listeners remain owned by Init until the service closes.
- ServerConfig supplies TLS, HTTP limits, a handler, a transport accounting
  wrapper and protocol shutdown hook. Handler() dispatches using the immutable
  request worker, preventing old requests from reaching new proxy generations.
- Worker.Close closes physical connections before protocol shutdown and joins
  Serve loops and active handlers, including hijacked handlers. TLS shutdown and
  blocked writes cannot prevent the protocol hook from completing.
- AcceptHandler, CloseHandler and go-service connection counts follow physical
  socket lifetime; hijacking does not decrement the live connection count.
- Context, Addresses, ActiveConnections and Err provide runtime observation.

The example without Configure remains valid. Run supplies a default worker for
its registered Gin routes. Managed mode uses FD 5 and listening FDs starting at 6,
requires one worker/prefork without reuseport/status notifications, and cleans up
unrelated inherited sockets without closing Go-owned CLOEXEC sockets.

Run this module's tests from the root module (vendor does not contain tests):

    go test -mod=mod -race github.com/acl-dev/gin-service -count=1
