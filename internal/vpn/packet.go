package vpn

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"time"
)

func ipv4(packet []byte, mtu int) (netip.Addr, netip.Addr, bool) {
	if len(packet) < 20 || len(packet) > mtu || packet[0]>>4 != 4 {
		return netip.Addr{}, netip.Addr{}, false
	}
	ihl := int(packet[0]&15) * 4
	if ihl < 20 || ihl > len(packet) || int(binary.BigEndian.Uint16(packet[2:4])) != len(packet) {
		return netip.Addr{}, netip.Addr{}, false
	}
	var a, b [4]byte
	copy(a[:], packet[12:16])
	copy(b[:], packet[16:20])
	return netip.AddrFrom4(a), netip.AddrFrom4(b), true
}
func publicDestination(ip netip.Addr) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !netip.MustParsePrefix("100.64.0.0/10").Contains(ip) && !netip.MustParsePrefix("198.18.0.0/15").Contains(ip)
}

type assembly struct {
	created time.Time
	total   int
	chunks  [2][]byte
}
type reassembler struct{ packets map[uint64]*assembly }

func (r *reassembler) push(data []byte, mtu int, now time.Time) ([]byte, error) {
	if len(data) < headerSize || data[0] != 1 || data[1] != 0 {
		return nil, errors.New("invalid VPN datagram")
	}
	id := binary.BigEndian.Uint64(data[2:10])
	total := int(binary.BigEndian.Uint16(data[10:12]))
	index, count := int(data[12]), int(data[13])
	expected := (total + fragmentPayload - 1) / fragmentPayload
	if total < 20 || total > mtu || count != expected || count < 1 || count > 2 || index >= count {
		return nil, errors.New("invalid VPN fragments")
	}
	size := fragmentPayload
	if index == count-1 {
		size = total - index*fragmentPayload
	}
	if len(data) != headerSize+size {
		return nil, errors.New("invalid VPN fragment length")
	}
	if count == 1 {
		return append([]byte(nil), data[headerSize:]...), nil
	}
	if r.packets == nil {
		r.packets = make(map[uint64]*assembly)
	}
	for k, p := range r.packets {
		if now.Sub(p.created) > 2*time.Second {
			delete(r.packets, k)
		}
	}
	p := r.packets[id]
	if p == nil {
		if len(r.packets) >= 64 {
			return nil, errors.New("VPN reassembly capacity exceeded")
		}
		p = &assembly{created: now, total: total}
		r.packets[id] = p
	}
	if p.total != total {
		return nil, errors.New("conflicting VPN fragments")
	}
	if p.chunks[index] != nil {
		return nil, nil
	}
	p.chunks[index] = append([]byte(nil), data[headerSize:]...)
	if p.chunks[0] == nil || p.chunks[1] == nil {
		return nil, nil
	}
	delete(r.packets, id)
	return append(p.chunks[0], p.chunks[1]...), nil
}
func fragments(packet []byte, id uint64, send func([]byte) error) error {
	count := (len(packet) + fragmentPayload - 1) / fragmentPayload
	for i := 0; i < count; i++ {
		part := packet[i*fragmentPayload : min(len(packet), (i+1)*fragmentPayload)]
		d := make([]byte, headerSize+len(part))
		d[0] = 1
		binary.BigEndian.PutUint64(d[2:10], id)
		binary.BigEndian.PutUint16(d[10:12], uint16(len(packet)))
		d[12] = byte(i)
		d[13] = byte(count)
		copy(d[headerSize:], part)
		if err := send(d); err != nil {
			return err
		}
	}
	return nil
}
