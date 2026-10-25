package cli

import (
	"flag"
	"time"
)

// NoisePolicyFlags 两端及配置校验使用同一套默认值；旧配置保留全场景发送行为。
func NoisePolicyFlags(fs *flag.FlagSet) {
	fs.Bool("noise-traffic", true, "send padding while application traffic is active")
	fs.Bool("noise-idle", true, "send padding while application streams are idle")
	fs.Bool("noise-empty", true, "send padding when no application streams exist")
	fs.Duration("noise-idle-after", time.Second, "no application data for this duration means idle (0 uses 1s)")
}
