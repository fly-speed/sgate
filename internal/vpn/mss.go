package vpn

import "encoding/binary"

// Keep ordinary TCP data packets within one legacy VPN DATAGRAM payload.
// MSS excludes the fixed IPv4/TCP headers (RFC 6691). Never raise an MSS.
func clampTCPMSS(packet []byte, mtu int) bool {
	if _, _, ok := ipv4(packet, mtu); !ok || packet[9] != 6 || binary.BigEndian.Uint16(packet[6:8])&0x3fff != 0 {
		return false
	}
	ipLen := int(packet[0]&15) * 4
	if len(packet) < ipLen+20 || packet[ipLen+13]&6 != 2 {
		return false
	}
	tcpLen := int(packet[ipLen+12]>>4) * 4
	if tcpLen < 20 || ipLen+tcpLen > len(packet) {
		return false
	}
	limit := uint16(min(mtu, fragmentPayload) - 40)
	position := -1
	// Validate the complete options before changing anything. Authentication
	// options cover TCP headers; modifying their MSS would invalidate them.
	for at, end := ipLen+20, ipLen+tcpLen; at < end; {
		kind := packet[at]
		if kind == 0 {
			break
		}
		if kind == 1 {
			at++
			continue
		}
		if at+2 > end {
			return false
		}
		size := int(packet[at+1])
		if size < 2 || at+size > end {
			return false
		}
		if kind == 19 || kind == 29 {
			return false
		}
		if kind == 2 {
			if size != 4 || position != -1 {
				return false
			}
			position = at + 2
		}
		at += size
	}
	if position < 0 {
		return false
	}
	old := binary.BigEndian.Uint16(packet[position : position+2])
	if old <= limit {
		return false
	}
	binary.BigEndian.PutUint16(packet[position:position+2], limit)
	// RFC 1624 incremental checksum update, including an odd-aligned option.
	before, after := old, limit
	if (position-ipLen)&1 != 0 {
		before = before<<8 | before>>8
		after = after<<8 | after>>8
	}
	checksum := binary.BigEndian.Uint16(packet[ipLen+16 : ipLen+18])
	sum := uint32(^checksum) + uint32(^before) + uint32(after)
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(packet[ipLen+16:ipLen+18], ^uint16(sum))
	return true
}
