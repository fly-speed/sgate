package main

import (
	"net/url"
	"testing"
)

func TestPublicAssetExtractionDoesNotExpandDestinations(t *testing.T) {
	base, _ := url.Parse("https://accounts.google.com/login")
	body := []byte(`<script src="https://www.gstatic.com/a.js?version=1"></script>
 <script src="https://www.gstatic.com/a.js?version=1"></script>
 <script src="http://www.gstatic.com/insecure.js"></script>
 <script src="https://untrusted.example/x.js"></script>
 <script src="https://name:secret@www.gstatic.com/private.js"></script>
 <script src="https://www.gstatic.com:8443/port.js"></script>
 <link rel="stylesheet" href="//www.gstatic.com/style.css">
 <script>window.location='https://untrusted.example'</script>`)
	got := collectAssets(body, base, map[string]string{"www.gstatic.com": "192.0.2.1"})
	if len(got) != 2 || got[0] != "https://www.gstatic.com/a.js?version=1" || got[1] != "https://www.gstatic.com/style.css" {
		t.Fatal(got)
	}
}

func TestPacketSummaryRecordsOnlyICMPMetadata(t *testing.T) {
	p := make([]byte, 56)
	p[0] = 0x45
	p[9] = 1
	copy(p[12:16], []byte{192, 0, 2, 1})
	p[20] = 3
	p[21] = 4
	p[26] = 5
	p[27] = 0
	p[28] = 0x45
	p[37] = 17
	copy(p[44:48], []byte{203, 0, 113, 1})
	s := &packetSummary{}
	s.record(p)
	got := s.snapshot()
	if got["icmp"] != 1 {
		t.Fatal(got)
	}
	if len(s.Errors) != 1 || s.Errors[0]["next_hop_mtu"] != uint16(1280) || s.Errors[0]["quoted_destination"] != "203.0.113.1" {
		t.Fatal(got)
	}
	for n := 0; n < 20; n++ {
		s.record(p[:n])
	}
	if len(s.Errors) != 1 {
		t.Fatal("malformed packet changed metadata")
	}
}
