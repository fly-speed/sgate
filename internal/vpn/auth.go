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
	"strings"
)

const probeChallengePrefix = "sgate-vpn-probe-v1:"
const authKeyMismatchCode = 0x101
const authInvalidHelloCode = 0x100
const accountChallengePrefix = assignmentChallengePrefix + "account-v1:"

type hello struct {
	Username     string `json:"username,omitempty"`
	AccountProof string `json:"account_proof,omitempty"`
	Padding      bool   `json:"padding,omitempty"`
	Probe        bool   `json:"probe,omitempty"`
	ID           string `json:"id"`
	IP           string `json:"ip"`
	Network      string `json:"network"`
	MTU          int    `json:"mtu"`
	Proof        string `json:"proof"`
}
type welcome struct {
	Padding            bool   `json:"padding,omitempty"`
	Probe              bool   `json:"probe,omitempty"`
	AddressInUse       bool   `json:"address_in_use,omitempty"`
	ClientLimitReached bool   `json:"client_limit_reached,omitempty"`
	Proof              string `json:"proof,omitempty"`
	IP                 string `json:"ip"`
	Gateway            string `json:"gateway"`
	Network            string `json:"network"`
	MTU                int    `json:"mtu"`
	Error              string `json:"error,omitempty"`
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
	parts := []any{Protocol, challenge, h.ID, h.IP, h.Network, h.MTU}
	if h.Probe {
		parts = append(parts, "probe-v1")
	}
	if h.Padding && !h.Probe {
		parts = append(parts, "padding-v1")
	}
	if h.Username != "" {
		parts = append(parts, "account-v1", h.Username)
	}
	b, _ := json.Marshal(parts)
	m.Write(b)
	return hex.EncodeToString(m.Sum(nil))
}
func signHello(c Config, challenge string, h *hello) error {
	if strings.HasPrefix(challenge, accountChallengePrefix) {
		if c.Username == "" || c.Password == "" {
			return errors.New("VPN server requires a username and password")
		}
		h.Username = c.Username
	} else if c.RequireAccount {
		return errors.New("VPN server did not request account authentication")
	}
	h.Proof = proof(c.Key, challenge, *h)
	if h.Username != "" {
		digest := sha256.Sum256([]byte(c.Password))
		mac := hmac.New(sha256.New, digest[:])
		mac.Write([]byte(h.Proof))
		h.AccountProof = hex.EncodeToString(mac.Sum(nil))
	}
	return nil
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
	parts := []any{Protocol, "server", challenge, h.ID, w.IP, w.Gateway, w.Network, w.MTU}
	if h.Probe {
		parts = append(parts, "probe-v1", w.Probe, w.AddressInUse, w.ClientLimitReached)
	}
	if h.Padding && !h.Probe {
		parts = append(parts, "padding-v1", w.Padding)
	}
	if h.Username != "" {
		parts = append(parts, "account-v1", h.Username, h.AccountProof)
	}
	b, _ := json.Marshal(parts)
	m.Write(b)
	return hex.EncodeToString(m.Sum(nil))
}

// NewClientIdentity creates a process-local identity reusable across endpoint switches.
func NewClientIdentity() (string, error) { return randomNonce() }
