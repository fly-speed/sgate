//go:build !unix

package master

// Descriptor ownership cleanup is supported on Unix acl_master deployments.
func closeInheritedSockets(listenerCount int) (int, error) { return 0, nil }
