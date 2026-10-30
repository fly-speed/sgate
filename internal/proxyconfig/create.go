package proxyconfig

import "sgate/internal/configfile"

// Create validates and publishes an initial mapping document without replacing
// existing storage, even when multiple client processes start concurrently.
func Create(path string, data []byte) (bool, error) {
	if _, err := Decode(data); err != nil {
		return false, err
	}
	return configfile.CreatePrivate(path, data)
}
