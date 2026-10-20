package tunnel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

const passwordHeader = "X-Sgate-Password"
const maxPasswordBytes = 1024

func validatePassword(password string) error {
	if len(password) == 0 || len(password) > maxPasswordBytes {
		return errors.New("password must contain 1 to 1024 bytes")
	}
	return nil
}

func passwordCipher(key []byte) cipher.AEAD {
	// Separate password-header encryption from both stream encryption directions.
	raw, _ := hex.DecodeString(mac(key, protocol, "password-header-key"))
	block, _ := aes.NewCipher(raw)
	aead, _ := cipher.NewGCM(block)
	return aead
}

func passwordAAD(key []byte, id, nonce, ts, resume, uri string) []byte {
	return []byte(mac(key, protocol, "password-context", id, nonce, ts, resume, uri))
}

func encryptPassword(key []byte, password, id, nonce, ts, resume, uri string) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}
	aead := passwordCipher(key)
	iv := make([]byte, aead.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", errors.New("cannot generate password encryption nonce")
	}
	sealed := aead.Seal(iv, iv, []byte(password), passwordAAD(key, id, nonce, ts, resume, uri))
	return "v1." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func verifyPassword(key []byte, password, token, id, nonce, ts, resume, uri string) bool {
	aead := passwordCipher(key)
	if !strings.HasPrefix(token, "v1.") || len(token) > 3+base64.RawURLEncoding.EncodedLen(aead.NonceSize()+maxPasswordBytes+aead.Overhead()) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token[3:])
	if err != nil || len(raw) < aead.NonceSize()+aead.Overhead()+1 {
		return false
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], passwordAAD(key, id, nonce, ts, resume, uri))
	if err != nil || len(plain) > maxPasswordBytes {
		return false
	}
	got, want := sha256.Sum256(plain), sha256.Sum256([]byte(password))
	for i := range plain {
		plain[i] = 0
	}
	return hmac.Equal(got[:], want[:])
}

func requestHeaders(key []byte, password, id, nonce, ts, resume, uri string) (http.Header, error) {
	token, err := encryptPassword(key, password, id, nonce, ts, resume, uri)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("X-Sgate-Session", id)
	h.Set("X-Sgate-Nonce", nonce)
	h.Set("X-Sgate-Time", ts)
	h.Set("X-Sgate-Resume", resume)
	h.Set(passwordHeader, token)
	h.Set("X-Sgate-Auth", mac(key, protocol, "client", id, nonce, ts, resume, uri, token))
	return h, nil
}
