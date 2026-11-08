// Package buildinfo reports release information for the service binaries.
package buildinfo

import (
	"fmt"
	"io"
)

const Version = "1.5.3"

// BuildTime is injected by the release build as a UTC RFC 3339 timestamp.
var BuildTime = "unknown"

func PrintRequested(out io.Writer, name string, args []string) bool {
	if len(args) == 0 {
		return false
	}
	verbose := false
	for _, arg := range args {
		switch arg {
		case "-v", "-version", "--version":
		case "-verbose", "--verbose":
			verbose = true
		default:
			// acl_master also passes -v alongside its service arguments.
			return false
		}
	}
	fmt.Fprintf(out, "%s %s\n", name, Version)
	if verbose {
		fmt.Fprintf(out, "Build time: %s\n", BuildTime)
	}
	return true
}
