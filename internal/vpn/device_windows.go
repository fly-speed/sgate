//go:build windows

package vpn

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wintun"
)

type windowsTUN struct {
	adapter *wintun.Adapter
	session wintun.Session
	event   windows.Handle
	mu      sync.RWMutex
	closed  atomic.Bool
	once    sync.Once
}

func OpenTUN(name string) (*TUN, error) {
	if name == "" {
		name = "sgateVPN"
	}
	if _, err := net.InterfaceByName(name); err == nil {
		return nil, fmt.Errorf("interface %s already exists; refusing to modify it", name)
	}
	a, err := wintun.CreateAdapter(name, "sgate", nil)
	if err != nil {
		return nil, fmt.Errorf("create Wintun adapter (run as administrator and place the matching official wintun.dll beside sgate-client.exe): %w", err)
	}
	s, err := a.StartSession(8 << 20)
	if err != nil {
		a.Close()
		return nil, err
	}
	b := &windowsTUN{adapter: a, session: s, event: s.ReadWaitEvent()}
	return &TUN{name: name, backend: b}, nil
}

func (d *windowsTUN) Close() error {
	d.once.Do(func() {
		d.closed.Store(true)
		windows.SetEvent(d.event)
		d.mu.Lock()
		defer d.mu.Unlock()
		d.session.End()
		d.adapter.Close()
	})
	return nil
}
func (d *windowsTUN) Read(p []byte) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for !d.closed.Load() {
		packet, err := d.session.ReceivePacket()
		if err == nil {
			n := copy(p, packet)
			d.session.ReleaseReceivePacket(packet)
			if n < len(packet) {
				return n, io.ErrShortBuffer
			}
			return n, nil
		}
		if !errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
			return 0, err
		}
		if _, err = windows.WaitForSingleObject(d.event, windows.INFINITE); err != nil {
			return 0, err
		}
	}
	return 0, os.ErrClosed
}
func (d *windowsTUN) Write(p []byte) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed.Load() {
		return 0, os.ErrClosed
	}
	if len(p) == 0 || len(p) > wintun.PacketSizeMax {
		return 0, io.ErrShortWrite
	}
	packet, err := d.session.AllocateSendPacket(len(p))
	if err != nil {
		return 0, err
	}
	copy(packet, p)
	d.session.SendPacket(packet)
	return len(p), nil
}
