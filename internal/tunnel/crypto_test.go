package tunnel

import (
	"bytes"
	"testing"
)

func TestCryptoAuthenticationAndReplay(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	client := newCodec(key, "session", "client nonce", "server nonce", true)
	server := newCodec(key, "session", "client nonce", "server nonce", false)
	sealed := client.seal([]byte("payload"))
	damaged := append([]byte(nil), sealed...)
	damaged[len(damaged)-1] ^= 1
	if _, err := server.open(damaged); err == nil {
		t.Fatal("tampered frame accepted")
	}
	plain, err := server.open(sealed)
	if err != nil || string(plain) != "payload" {
		t.Fatalf("decrypt: %q %v", plain, err)
	}
	if _, err := server.open(sealed); err == nil {
		t.Fatal("replayed frame accepted")
	}
	if _, err := client.open(client.seal([]byte("reflection"))); err == nil {
		t.Fatal("reflected frame accepted")
	}
	other := newCodec(key, "session", "client nonce", "different server nonce", false)
	if _, err := other.open(sealed); err == nil {
		t.Fatal("old transport frame accepted on new transport")
	}
}
func TestFrameValidation(t *testing.T) {
	good := frame{op: opData, seq: 1, ack: 2, stream: 3, data: []byte("hello")}
	f, err := decodeFrame(good.encode())
	if err != nil || f.seq != 1 || string(f.data) != "hello" {
		t.Fatal(f, err)
	}
	for _, bad := range []frame{{op: 99}, {op: opAck, seq: 1}, {op: opOpen, seq: 1}, {op: opData, seq: 1, stream: 1}, {op: opFin, seq: 1, stream: 1, data: []byte{1}}} {
		if _, err := decodeFrame(bad.encode()); err == nil {
			t.Fatalf("accepted malformed frame: %+v", bad)
		}
	}
}
