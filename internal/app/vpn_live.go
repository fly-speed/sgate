package app

import (
	"flag"
	"sgate/internal/cli"
	"sgate/internal/vpn"
	"slices"
)

// Read only submitted live values. Pending mode, endpoint, credentials or route
// edits must never be pulled into an existing VPN by an unrelated live save.
func (c *controller) prepareVPNLive(keys []string) (*vpn.Prepared, *vpn.LiveConfig, bool, error) {
	if c.current.vpn == nil {
		return nil, nil, false, nil
	}
	explicit := map[string]bool{}
	fs := cli.CloneFlags(c.original)
	fs.String("config", "", "")
	if err := fs.Parse(c.args); err != nil {
		return nil, nil, false, err
	}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	selected := []string{}
	for _, key := range keys {
		if slices.Contains(vpn.LiveFields(c.role), key) && !slices.Contains(c.overrides, key) && !explicit[key] {
			selected = append(selected, key)
		}
	}
	endpoint := c.role == "client" && slices.Contains(keys, "vpn-server") && !slices.Contains(c.overrides, "vpn-server") && !explicit["vpn-server"]
	if len(selected) == 0 && !endpoint {
		return nil, nil, false, nil
	}
	values, err := cli.ReadValues(c.path)
	if err != nil {
		return nil, nil, false, err
	}
	live, err := vpn.WithLiveValues(vpn.LiveFromConfig(c.current.vpn.Config), values, selected)
	if err != nil {
		return nil, nil, false, err
	}
	next := c.current.vpn.WithLiveConfig(live)
	if endpoint {
		next, err = next.WithServerURL(values["vpn-server"])
		if err != nil {
			return nil, nil, false, err
		}
	}
	return next, &live, endpoint && next.Fingerprint != c.current.vpn.Fingerprint, nil
}
