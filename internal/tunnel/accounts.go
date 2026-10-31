package tunnel

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxAccounts = 4096
const maxAccountsFileBytes = 1024 * 1024
const maxUsernameBytes = 128
const maxAccountCredentialBytes = 8192

// Account is a tunnel identity, independent of Web management login users.
// Protect the local JSON file as a secret, just like the legacy password .cf.
type Account struct {
	Username        string           `json:"username"`
	Password        string           `json:"password"`
	Disabled        bool             `json:"disabled,omitempty"`
	ReverseMappings []ReverseMapping `json:"reverse_mappings,omitempty"`
}
type accountVerifier struct {
	digest   [sha256.Size]byte
	disabled bool
	reverse  []ReverseMapping
}

func validateUsername(username string) error {
	if username == "" || len(username) > maxUsernameBytes || !utf8.ValidString(username) {
		return errors.New("username must contain 1 to 128 UTF-8 bytes")
	}
	for _, c := range username {
		if unicode.IsSpace(c) || unicode.IsControl(c) {
			return errors.New("username must not contain whitespace or control characters")
		}
	}
	return nil
}

// LoadAccounts fails closed on unreadable, oversized or malformed files. No
// account values are included in errors, including JSON decoder failures.
func LoadAccounts(path string) ([]Account, error) {
	// Check before Open too: opening a FIFO could otherwise block a settings save.
	info, err := os.Stat(path)
	if err != nil {
		return nil, errors.New("cannot open tunnel accounts file")
	}
	if !info.Mode().IsRegular() || info.Size() > maxAccountsFileBytes {
		return nil, errors.New("tunnel accounts file must be a regular file up to 1 MiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open tunnel accounts file")
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxAccountsFileBytes {
		return nil, errors.New("tunnel accounts file must be a regular file up to 1 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxAccountsFileBytes+1))
	if err != nil || len(data) > maxAccountsFileBytes || !utf8.Valid(data) {
		return nil, errors.New("cannot read valid UTF-8 tunnel accounts file")
	}
	return DecodeAccounts(data)
}

// DecodeAccounts validates the shared on-disk account document format.
func DecodeAccounts(data []byte) ([]Account, error) {
	if len(data) > maxAccountsFileBytes || !utf8.Valid(data) {
		return nil, errors.New("invalid tunnel accounts JSON size or encoding")
	}
	var document struct {
		Version  int       `json:"version"`
		Accounts []Account `json:"accounts"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("invalid tunnel accounts JSON")
	}
	if decoder.Decode(new(any)) != io.EOF || document.Version != 1 {
		return nil, errors.New("tunnel accounts JSON requires version 1 and a single document")
	}
	if _, err := accountVerifiers(document.Accounts); err != nil {
		return nil, err
	}
	return document.Accounts, nil
}

func accountVerifiers(accounts []Account) (map[string]accountVerifier, error) {
	if accounts == nil || len(accounts) > maxAccounts {
		return nil, errors.New("tunnel accounts requires an array of up to 4096 entries")
	}
	if err := validateReverseMappings(accounts); err != nil {
		return nil, err
	}
	verifiers := make(map[string]accountVerifier, len(accounts))
	for _, account := range accounts {
		if err := validateUsername(account.Username); err != nil {
			return nil, err
		}
		if err := validatePassword(account.Password); err != nil {
			return nil, err
		}
		if _, exists := verifiers[account.Username]; exists {
			return nil, errors.New("duplicate tunnel account username")
		}
		verifiers[account.Username] = accountVerifier{digest: sha256.Sum256([]byte(account.Password)), disabled: account.Disabled, reverse: append([]ReverseMapping(nil), account.ReverseMappings...)}
	}
	return verifiers, nil
}

// v2 encrypts both identity and password, while v1 remains byte-for-byte
// compatible with existing unnamed clients and servers. The token is already
// bound to the request URI, session, nonce, timestamp and handshake HMAC.
func encryptAccount(key []byte, username, password, id, nonce, ts, resume, uri string) (string, error) {
	if err := validateUsername(username); err != nil {
		return "", err
	}
	if err := validatePassword(password); err != nil {
		return "", err
	}
	plain, _ := json.Marshal(Account{Username: username, Password: password})
	defer clear(plain)
	aead := passwordCipher(key)
	iv := make([]byte, aead.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", errors.New("cannot generate account encryption nonce")
	}
	aad := append([]byte("account-v2:"), passwordAAD(key, id, nonce, ts, resume, uri)...)
	token := aead.Seal(iv, iv, plain, aad)
	return "v2." + base64.RawURLEncoding.EncodeToString(token), nil
}

func (s *Server) verifyAccount(token, id, nonce, ts, resume, uri string) (string, bool) {
	username, _, ok := s.authenticateAccount(token, id, nonce, ts, resume, uri)
	return username, ok
}

func (s *Server) authenticateAccount(token, id, nonce, ts, resume, uri string) (string, [sha256.Size]byte, bool) {
	if !strings.HasPrefix(token, "v2.") || len(token) > 3+base64.RawURLEncoding.EncodedLen(maxAccountCredentialBytes) {
		return "", [sha256.Size]byte{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token[3:])
	aead := passwordCipher(s.cfg.Key)
	if err != nil || len(raw) < aead.NonceSize()+aead.Overhead() {
		return "", [sha256.Size]byte{}, false
	}
	aad := append([]byte("account-v2:"), passwordAAD(s.cfg.Key, id, nonce, ts, resume, uri)...)
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], aad)
	if err != nil {
		return "", [sha256.Size]byte{}, false
	}
	defer clear(plain)
	var credential Account
	if err := json.Unmarshal(plain, &credential); err != nil || validateUsername(credential.Username) != nil || validatePassword(credential.Password) != nil {
		return "", [sha256.Size]byte{}, false
	}
	s.mu.Lock()
	account, exists := s.accounts[credential.Username]
	s.mu.Unlock()
	digest := sha256.Sum256([]byte(credential.Password))
	matched := hmac.Equal(account.digest[:], digest[:])
	return credential.Username, digest, exists && !account.disabled && matched
}

// EncodeAccounts is used by the authenticated management writer; passwords
// stay on disk and are never part of its list responses.
func EncodeAccounts(accounts []Account) ([]byte, error) {
	if _, err := accountVerifiers(accounts); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(struct {
		Version  int       `json:"version"`
		Accounts []Account `json:"accounts"`
	}{1, accounts}, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(data) > maxAccountsFileBytes {
		return nil, errors.New("tunnel accounts JSON exceeds 1 MiB")
	}
	return data, nil
}

// UpdateAccounts atomically replaces authentication policy and revokes only
// sessions whose account was removed, disabled, or had its password or mappings changed.
func (s *Server) UpdateAccounts(accounts []Account) error {
	next, err := accountVerifiers(accounts)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("tunnel account authentication is not running")
	}
	if s.reverseStarted {
		if err := s.reconcileReverseLocked(next, nil); err != nil {
			return err
		}
	}
	previous := s.accounts
	s.accountMode = true
	s.accounts = next
	for id, entry := range s.sessions {
		account, exists := next[entry.username]
		old := previous[entry.username]
		if !exists || account.disabled || account.digest != old.digest || !sameReverseMappings(account.reverse, old.reverse) {
			entry.session.close()
			delete(s.sessions, id)
		}
	}
	return nil
}

// UseLegacyAuthentication explicitly disables file-based authentication. Only
// the already-running legacy password is used; pending password edits stay pending.
func (s *Server) UseLegacyAuthentication() error {
	if err := validatePassword(s.cfg.Password); err != nil {
		return errors.New("清空账号文件前需先配置并应用有效的公共隧道密码")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("tunnel account authentication is not running")
	}
	if !s.accountMode {
		return nil
	}
	s.closeReverseLocked()
	s.accountMode = false
	s.accounts = nil
	for id, entry := range s.sessions {
		entry.session.close()
		delete(s.sessions, id)
	}
	return nil
}

// Recheck after authentication so a request waiting on upgrade/admission cannot
// enter using a policy that was revoked by an online configuration change.
func (s *Server) authorizedLocked(request tunnelRequest) bool {
	if !s.accountMode {
		return request.username == ""
	}
	account, exists := s.accounts[request.username]
	return exists && !account.disabled && account.digest == request.accountDigest
}
