package vpn

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func testChecksum(data []byte) uint16 {
	var sum uint32
	for len(data) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(data))
		data = data[2:]
	}
	if len(data) > 0 {
		sum += uint32(data[0]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 65535) + (sum >> 16)
	}
	return ^uint16(sum)
}
func testTCPChecksum(packet []byte) uint16 {
	ihl := int(packet[0]&15) * 4
	pseudo := append([]byte(nil), packet[12:20]...)
	pseudo = append(pseudo, 0, 6, byte((len(packet)-ihl)>>8), byte(len(packet)-ihl))
	return testChecksum(append(pseudo, packet[ihl:]...))
}
func tcpSYN(options []byte, ipOptions bool, flags byte, data []byte) []byte {
	ihl := 20
	if ipOptions {
		ihl = 24
	}
	padded := (len(options) + 3) / 4 * 4
	p := make([]byte, ihl+20+padded+len(data))
	p[0] = 0x40 | byte(ihl/4)
	p[8] = 64
	p[9] = 6
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	copy(p[12:20], []byte{10, 77, 0, 2, 8, 8, 8, 8})
	binary.BigEndian.PutUint16(p[ihl:ihl+2], 1000)
	binary.BigEndian.PutUint16(p[ihl+2:ihl+4], 443)
	p[ihl+12] = byte((20+padded)/4) << 4
	p[ihl+13] = flags
	copy(p[ihl+20:], options)
	copy(p[ihl+20+padded:], data)
	binary.BigEndian.PutUint16(p[10:12], testChecksum(p[:ihl]))
	binary.BigEndian.PutUint16(p[ihl+16:ihl+18], testTCPChecksum(p))
	return p
}
func TestVPNMSSChecksumAndOptions(t *testing.T) {
	for _, ipOpts := range []bool{false, true} {
		for _, prefix := range [][]byte{nil, {1}, {1, 1, 1}} {
			for _, flags := range []byte{2, 0x12} {
				p := tcpSYN(append(append([]byte(nil), prefix...), 2, 4, 0x05, 0xb4), ipOpts, flags, []byte{7, 8, 9})
				ihl := int(p[0]&15) * 4
				originalIP := append([]byte(nil), p[:ihl]...)
				if testTCPChecksum(p) != 0 || !clampTCPMSS(p, 1280) {
					t.Fatal("SYN not clamped")
				}
				if testTCPChecksum(p) != 0 {
					t.Fatalf("invalid TCP checksum (IP options=%t, alignment=%d)", ipOpts, len(prefix))
				}
				if !bytes.Equal(originalIP, p[:ihl]) {
					t.Fatal("IPv4 header changed")
				}
				if got := binary.BigEndian.Uint16(p[ihl+22+len(prefix):]); got != 960 {
					t.Fatal(got)
				}
				if clampTCPMSS(p, 1280) {
					t.Fatal("MSS rewrite is not idempotent")
				}
			}
		}
	}
	p := tcpSYN([]byte{2, 4, 5, 180}, false, 2, nil)
	if !clampTCPMSS(p, 576) || binary.BigEndian.Uint16(p[42:44]) != 536 || testTCPChecksum(p) != 0 {
		t.Fatal("small configured MTU ignored")
	}
}
func TestVPNMSSLeavesUnsupportedAndMalformedPacketsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		options  []byte
		flags    byte
		fragment bool
	}{
		{[]byte{2, 4, 2, 24}, 2, false},     // Already smaller than the cap.
		{[]byte{2, 4, 5, 180}, 0x10, false}, // Ordinary ACK.
		{[]byte{2, 4, 5, 180}, 2, true},
		{[]byte{2, 3, 5, 180}, 2, false},
		{[]byte{2, 4, 5, 180, 3, 8, 0, 0}, 2, false},   // Truncated option after MSS.
		{[]byte{2, 4, 5, 180, 2, 4, 5, 180}, 2, false}, // Duplicate MSS.
		{[]byte{2, 4, 5, 180, 19, 2}, 2, false},        // TCP MD5 option.
		{[]byte{2, 4, 5, 180, 29, 2}, 2, false},        // TCP AO option.
		{[]byte{1, 1, 0, 0}, 2, false},
	} {
		p := tcpSYN(tc.options, false, tc.flags, nil)
		if tc.fragment {
			p[6] = 0x20
		}
		before := append([]byte(nil), p...)
		if clampTCPMSS(p, 1280) || !bytes.Equal(before, p) {
			t.Fatal("unsupported packet changed", tc)
		}
	}
	p := tcpSYN([]byte{2, 4, 5, 180}, false, 2, nil)
	for n := 0; n < len(p); n++ {
		if clampTCPMSS(p[:n], 1280) {
			t.Fatal("truncated packet changed")
		}
	}
}
func TestVPNReassemblyCapacityRecoversWithoutWaitingForTimeout(t *testing.T) {
	stats := &Stats{}
	r := reassembler{stats: stats}
	now := time.Now()
	packet := ipPacket("8.8.8.8", "10.77.0.2", 1280)
	collect := func(id uint64) [][]byte {
		var result [][]byte
		fragments(packet, id, func(b []byte) error { result = append(result, b); return nil })
		return result
	}
	for id := uint64(0); id < 64; id++ {
		parts := collect(id)
		r.push(parts[0], 1280, now)
	}
	fresh := collect(64)
	if _, err := r.push(fresh[0], 1280, now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	actual, err := r.push(fresh[1], 1280, now.Add(2*time.Millisecond))
	if err != nil || !bytes.Equal(actual, packet) || len(r.packets) > 64 || stats.Snapshot().FragmentEvicted != 1 {
		t.Fatal("full table stalled fresh traffic", err, stats.Snapshot())
	}
	if _, exists := r.packets[0]; exists {
		t.Fatal("oldest incomplete assembly retained")
	}
	r.push(collect(65)[0], 1280, now.Add(3*time.Second))
	if len(r.packets) != 1 || stats.Snapshot().FragmentExpired != 63 {
		t.Fatal("expired assemblies retained", stats.Snapshot())
	}
}
func TestVPNMSSAppliedOnRealQUICBothDirections(t *testing.T) {
	_, cfg, remote := fixture(t, 4)
	local, _, _ := startClient(t, cfg)
	await(t, "VPN connected", func() bool { return cfg.Stats.Snapshot().Active == 1 })
	packet := tcpSYN([]byte{2, 4, 5, 180}, false, 2, nil)
	want := append([]byte(nil), packet...)
	clampTCPMSS(want, 1280)
	local.out <- packet
	receivePacket(t, remote, want)
	reply := tcpSYN([]byte{2, 4, 5, 180}, false, 0x12, nil)
	copy(reply[12:16], packet[16:20])
	copy(reply[16:20], packet[12:16])
	reply[10] = 0
	reply[11] = 0
	binary.BigEndian.PutUint16(reply[10:12], testChecksum(reply[:20]))
	reply[36] = 0
	reply[37] = 0
	binary.BigEndian.PutUint16(reply[36:38], testTCPChecksum(reply))
	want = append([]byte(nil), reply...)
	clampTCPMSS(want, 1280)
	remote.out <- reply
	receivePacket(t, local, want)
}
