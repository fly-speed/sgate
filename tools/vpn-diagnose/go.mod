module sgate/tools/vpn-diagnose

go 1.24.0

require (
	github.com/quic-go/quic-go v0.54.0
	golang.org/x/net v0.49.0
	golang.zx2c4.com/wireguard v0.0.0-20250521234502-f333402bd9cb
	sgate v0.0.0
)

require (
	github.com/acl-dev/go-service v1.1.2 // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/pion/dtls/v3 v3.1.15 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/transport/v5 v5.0.0 // indirect
	github.com/quic-go/qpack v0.5.1 // indirect
	golang.org/x/crypto v0.48.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/text v0.34.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	gvisor.dev/gvisor v0.0.0-20250503011706-39ed1f5ac29c // indirect
)

replace sgate => ../..
