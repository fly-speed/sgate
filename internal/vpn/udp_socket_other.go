//go:build !darwin && !linux

package vpn

import "net"

func udpListenConfig() net.ListenConfig { return net.ListenConfig{} }
