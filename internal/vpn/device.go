package vpn

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
)

type TUN struct {
	file            *os.File
	name            string
	framed          bool
	readMu, writeMu sync.Mutex
	buffer          []byte
}

func (d *TUN) Name() string { return d.name }
func (d *TUN) Close() error { return d.file.Close() }
func (d *TUN) Read(p []byte) (int, error) {
	if !d.framed {
		return d.file.Read(p)
	}
	d.readMu.Lock()
	defer d.readMu.Unlock()
	if d.buffer == nil {
		d.buffer = make([]byte, 65539)
	}
	n, err := d.file.Read(d.buffer)
	if err != nil {
		return 0, err
	}
	if n < 4 {
		return 0, errors.New("short utun packet")
	}
	if n-4 > len(p) {
		return 0, io.ErrShortBuffer
	}
	copy(p, d.buffer[4:n])
	return n - 4, nil
}
func (d *TUN) Write(p []byte) (int, error) {
	if !d.framed {
		return d.file.Write(p)
	}
	if len(p) == 0 {
		return 0, io.ErrShortWrite
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	packet := make([]byte, 4+len(p))
	family := uint32(2)
	if p[0]>>4 == 6 {
		family = 30
	}
	binary.BigEndian.PutUint32(packet, family)
	copy(packet[4:], p)
	n, err := d.file.Write(packet)
	if n < 4 {
		return 0, err
	}
	return n - 4, err
}
