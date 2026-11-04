package vpn

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

type udpReliableControl struct {
	incoming chan []byte
	acks     chan uint32
	received uint32 // owned by the datagram reader
	sendMu   sync.Mutex
	sent     uint32
}

func (c *udpConnection) deliverUDPControl() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case message := <-c.reliable.incoming:
			if message == nil {
				c.writer.Close()
				return
			}
			if _, err := c.writer.Write(message); err != nil {
				return
			}
		}
	}
}

func (c *udpConnection) receiveUDPControl(payload []byte) {
	if len(payload) <= 4 {
		return
	}
	control := c.reliable
	sequence := binary.BigEndian.Uint32(payload)
	if sequence == control.received+1 {
		select {
		case control.incoming <- append([]byte(nil), payload[4:]...):
			control.received = sequence
		default:
			return
		}
	}
	if sequence != 0 && sequence == control.received {
		_ = c.write(udpControlACK, payload[:4])
	}
}

func (c *udpConnection) writeUDPControl(message []byte) (int, error) {
	if len(message) == 0 {
		return 0, nil
	}
	if len(message) > udpFrameMax-udpHeaderSize-4 {
		return 0, errors.New("UDP VPN control message too large")
	}
	control := c.reliable
	control.sendMu.Lock()
	defer control.sendMu.Unlock()
	control.sent++
	sequence := control.sent
	payload := make([]byte, 4+len(message))
	binary.BigEndian.PutUint32(payload, sequence)
	copy(payload[4:], message)
	for _, interval := range []time.Duration{400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond} {
		if err := c.write(udpControl, payload); err != nil {
			return 0, err
		}
		c.writeMu.Lock()
		deadline := c.writeDeadline
		c.writeMu.Unlock()
		if !deadline.IsZero() {
			interval = min(interval, time.Until(deadline))
		}
		confirmed, err := c.waitUDPControlACK(sequence, interval)
		if err != nil {
			return 0, err
		}
		if confirmed {
			return len(message), nil
		}
	}
	return 0, os.ErrDeadlineExceeded
}

func (c *udpConnection) waitUDPControlACK(sequence uint32, interval time.Duration) (bool, error) {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return false, io.EOF
		case <-timer.C:
			return false, nil
		case ack := <-c.reliable.acks:
			if ack == sequence {
				return true, nil
			}
		}
	}
}
