package vpn

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
)

type hello struct {
	ID      string `json:"id"`
	IP      string `json:"ip"`
	Network string `json:"network"`
	MTU     int    `json:"mtu"`
	Proof   string `json:"proof"`
}
type welcome struct {
	Proof   string `json:"proof,omitempty"`
	IP      string `json:"ip"`
	Gateway string `json:"gateway"`
	Network string `json:"network"`
	MTU     int    `json:"mtu"`
	Error   string `json:"error,omitempty"`
}

func randomNonce() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func proof(key []byte, challenge string, h hello) string {
	m := hmac.New(sha256.New, key)
	// Fixed order and JSON encoding avoid ambiguous concatenation.
	b, _ := json.Marshal([]any{Protocol, challenge, h.ID, h.IP, h.Network, h.MTU})
	m.Write(b)
	return hex.EncodeToString(m.Sum(nil))
}
func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > 4096 {
		return errors.New("VPN control message too large")
	}
	p := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(p, uint32(len(b)))
	copy(p[4:], b)
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrNoProgress
		}
		p = p[n:]
	}
	return nil
}
func readJSON(r io.Reader, v any) error {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n == 0 || n > 4096 {
		return errors.New("invalid VPN control length")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func welcomeProof(key []byte, challenge string, h hello, w welcome) string {
	m := hmac.New(sha256.New, key)
	b, _ := json.Marshal([]any{Protocol, "server", challenge, h.ID, w.IP, w.Gateway, w.Network, w.MTU})
	m.Write(b)
	return hex.EncodeToString(m.Sum(nil))
}
