package tunnel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

const protocol = "sgate-v1"

// ParseKey requires a randomly generated 256-bit shared secret.
func ParseKey(value string) ([]byte, error) {
	key, err := hex.DecodeString(value)
	if err != nil || len(key) != 32 {
		return nil, errors.New("key must be 64 hexadecimal characters (32 random bytes)")
	}
	return key, nil
}

func randomID() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func validID(s string) bool { b, e := hex.DecodeString(s); return e == nil && len(b) == 32 }

func mac(key []byte, parts ...string) string {
	h := hmac.New(sha256.New, key)
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func freshTimestamp(value string) bool {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return false
	}
	now := time.Now().Unix()
	return n >= now-60 && n <= now+60
}

type codec struct {
	tx, rx         cipher.AEAD
	sent, received uint64
}

func newCodec(key []byte, sessionID, clientNonce, serverNonce string, client bool) *codec {
	derive := func(direction string) cipher.AEAD {
		raw, _ := hex.DecodeString(mac(key, protocol, sessionID, clientNonce, serverNonce, direction))
		block, _ := aes.NewCipher(raw)
		aead, _ := cipher.NewGCM(block)
		return aead
	}
	c := &codec{tx: derive("client-to-server"), rx: derive("server-to-client")}
	if !client {
		c.tx, c.rx = c.rx, c.tx
	}
	return c
}

func (c *codec) seal(p []byte) []byte {
	c.sent++
	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], c.sent)
	header := make([]byte, 8)
	binary.BigEndian.PutUint64(header, c.sent)
	return c.tx.Seal(header, nonce[:], p, []byte(protocol))
}

func (c *codec) open(p []byte) ([]byte, error) {
	if len(p) < 8+c.rx.Overhead() {
		return nil, errors.New("short encrypted frame")
	}
	n := binary.BigEndian.Uint64(p)
	if n != c.received+1 {
		return nil, fmt.Errorf("unexpected encryption counter %d", n)
	}
	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], n)
	plain, err := c.rx.Open(nil, nonce[:], p[8:], []byte(protocol))
	if err == nil {
		c.received = n
	}
	return plain, err
}
