package app

import (
	"context"
	"strings"
	"time"

	"sgate/internal/cli"
	"sgate/internal/vpn"
)

func (c *controller) testVPN(parent context.Context, fields map[string]string) (vpn.ProbeResult, error) {
	c.mu.Lock()
	fs := cli.CloneFlags(c.original)
	args := append([]string(nil), c.args...)
	processCtx := c.ctx
	overrides := map[string]bool{}
	for _, key := range c.overrides {
		overrides[key] = true
	}
	c.mu.Unlock()
	started := time.Now()
	fail := func(stage string, err error) (vpn.ProbeResult, error) {
		return vpn.ProbeResult{Stage: stage, Error: err.Error(), ElapsedMS: time.Since(started).Milliseconds()}, nil
	}
	// Test settings only: this local flag set does not change the running mode.
	// Force parsing dormant file VPN values, then apply draft values except CLI
	// overrides, which retain the same priority as normal application startup.
	for key, value := range fields {
		if key != "vpn-mode" && !overrides[key] {
			args = append(args, "-"+key+"="+value)
		}
	}
	args = append(args, "-vpn-mode=both")
	if err := cli.Parse(fs, args); err != nil {
		return fail("config", err)
	}
	p, err := vpn.Prepare("client", fs)
	if err != nil {
		return fail("config", err)
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if processCtx != nil {
		stop := context.AfterFunc(processCtx, cancel)
		defer stop()
	}
	config, _, err := p.Resolve(ctx)
	if err != nil {
		return fail("resolve", err)
	}
	result := vpn.Probe(ctx, config)
	result.ElapsedMS = time.Since(started).Milliseconds()
	// Preserve the configured hostname for a useful certificate diagnostic.
	if u := fs.Lookup("vpn-server"); u != nil {
		result.Target = strings.TrimSpace(u.Value.String())
	}
	return result, nil
}
