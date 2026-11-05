package vpn

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"path/filepath"

	"sgate/internal/tunnel"
)

// LoadAccountFile loads a snapshot of the VPN-only credentials. The client
// file contains one account; the server file is an allowlist.
func (c *Config) LoadAccountFile(path string, server bool) error {
	if path == "" {
		if c.RequireAccount {
			return errors.New("VPN account authentication requires a dedicated account file")
		}
		return nil
	}
	accounts, err := tunnel.LoadAccounts(path)
	if err != nil {
		return err
	}
	return c.setAccounts(path, accounts, server)
}

func (c *Config) setAccounts(path string, accounts []tunnel.Account, server bool) error {
	var err error
	for _, account := range accounts {
		if len(account.ReverseMappings) != 0 {
			return errors.New("VPN account files do not support reverse mappings")
		}
	}
	if server {
		c.VerifyAccount, err = tunnel.NewAccountProofVerifier(accounts)
		if err != nil {
			return err
		}
	} else {
		if len(accounts) != 1 || accounts[0].Disabled {
			return errors.New("VPN client credentials must contain exactly one enabled account")
		}
		c.Username, c.Password = accounts[0].Username, accounts[0].Password
	}
	c.accountPath = filepath.Clean(path)
	c.RequireAccount = true
	data, err := json.Marshal(accounts)
	if err != nil {
		return err
	}
	c.accountRevision = sha256.Sum256(data)
	clear(data)
	return nil
}

func (p *Prepared) WithServerAccounts(path string, accounts []tunnel.Account) (*Prepared, error) {
	next := *p
	if err := next.Config.setAccounts(path, accounts, true); err != nil {
		return nil, err
	}
	next.Fingerprint = next.serverFingerprint()
	return &next, nil
}
