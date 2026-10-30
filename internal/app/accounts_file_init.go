package app

import (
	"fmt"
	"log"
	"os"

	"sgate/internal/cli"
	"sgate/internal/configfile"
	"sgate/internal/tunnel"
)

// Initialization belongs to startup, keeping configuration validation and live
// file switching read-only. An empty list enables account mode and denies all
// clients until an administrator adds an account; it never enables legacy auth.
func (c *controller) initializeAccountsFile() error {
	fs := cli.CloneFlags(c.original)
	if err := cli.Parse(fs, c.args); err != nil {
		return err
	}
	path := (configValues{flags: fs}).accountPath()
	if path == "" {
		return nil
	}
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("cannot inspect tunnel accounts file: %w", err)
	}
	data, err := tunnel.EncodeAccounts([]tunnel.Account{})
	if err != nil {
		return err
	}
	created, err := configfile.CreatePrivate(path, append(data, '\n'))
	if err != nil {
		return err
	}
	if created {
		log.Printf("created tunnel accounts file: path=%q accounts=0; add client accounts through Web management", path)
	}
	return nil
}
