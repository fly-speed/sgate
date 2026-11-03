package app

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"sgate/internal/cli"
	"sgate/internal/proxyconfig"
)

// Only the startup path initializes missing storage. load/build stay read-only,
// including management validation, probes and online configuration changes.
func (c *controller) initializeProxyFile() error {
	fs := cli.CloneFlags(c.original)
	if err := cli.Parse(fs, c.args); err != nil {
		return err
	}
	if f := fs.Lookup("vpn-mode"); f != nil && f.Value.String() == "only" {
		return nil
	}
	values := configValues{flags: fs}
	path := proxyconfig.Path(values.optionalText("config"), values.optionalText("proxies-file"))
	if path == "" {
		return nil
	}
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("cannot inspect proxy mapping file: %w", err)
	}
	raw := strings.TrimSpace(values.optionalText("proxies"))
	if raw == "" {
		raw = "[]"
	}
	mappings, err := proxyconfig.Decode([]byte(raw))
	if err != nil {
		return err
	}
	if len(mappings) > 0 {
		// Validate seed listeners, URLs and effective credentials before persisting.
		_ = fs.Set("proxies-file", "")
		if _, err := build("client", fs, nil); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(mappings, "", "  ")
	if err != nil {
		return err
	}
	created, err := proxyconfig.Create(path, append(data, '\n'))
	if err != nil {
		return err
	}
	if created {
		log.Printf("created proxy mapping file: path=%q mappings=%d", path, len(mappings))
	}
	return nil
}
