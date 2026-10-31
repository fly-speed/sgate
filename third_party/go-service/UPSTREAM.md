# Local TCP lifecycle extension

Based on github.com/acl-dev/go-service v1.1.2 (original license preserved).
The upstream reference checkout in vendor/go-service remains unchanged.

The original no-option TcpServiceInit API remains available. WithTCPContext and
WithTCPManaged opt into an owned lifecycle: contextual framework monitoring,
Close with joined accept/connection callbacks, and restartable Configure workers.
TcpService.Run still owns the physical accept loop and invokes AcceptHandler and
CloseHandler. AcceptConn adapts callbacks to a disposable listener for stream-based
applications and keeps the framework connection counter valid for the full stream.

Managed listeners stay owned by acl_master. Standalone listener replacement uses
exclusive net.Listen and supports rollback after a failed rebind. Framework state
FD 5 and inherited sockets are closed on shutdown; unrelated inherited sockets are
removed before acquisition. No global Stop/Wait channel is used by this opt-in API.

UpdateAddresses transactionally binds standalone additions before committing the
listener set, retaining the current worker, unchanged accept loops and accepted
connections. Removed accept loops are joined individually; wildcard and port-zero
identities are retained. Managed listener updates still require acl_master to
replace its sockets; the application exposes that as an automatic service restart.

Managed contextual services acknowledge ACL_MASTER_STAT_START_OK on the owned status channel when Run starts, after application initialization. The native 12-byte message carries PID, the octal GENERATION environment value and status 4. This startup acknowledgement is independent of busy/idle notifications.
