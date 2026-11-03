//go:build darwin || linux

package tunnel

import (
	"runtime"
	"syscall"
)

func baselineUsage() (float64, uint64) {
	var r syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r)
	cpu := float64(r.Utime.Sec+r.Stime.Sec) + float64(r.Utime.Usec+r.Stime.Usec)/1e6
	rss := uint64(r.Maxrss)
	if runtime.GOOS == "linux" {
		rss *= 1024
	}
	return cpu, rss
}
