package app

import (
	"errors"
	"net"
	"strings"

	"sgate/internal/tunnel"
)

// master_service also includes sockets reserved for reverse TCP mappings.
// Those are leased by the tunnel server, never exposed as HTTP endpoints.
func serverHTTPListeners(addresses string, accounts []tunnel.Account) (string, error) {
	reserved := map[string]bool{}
	for _, account := range accounts {
		for _, mapping := range account.ReverseMappings {
			reserved[mapping.Listen] = true
		}
	}
	var http []string
	for _, address := range strings.FieldsFunc(addresses, func(r rune) bool { return r == ',' || r == ';' }) {
		address = strings.ReplaceAll(strings.TrimSpace(address), "|", ":")
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return "", err
		}
		address = net.JoinHostPort(host, port)
		if !reserved[address] {
			http = append(http, address)
		}
	}
	if len(http) == 0 {
		return "", errors.New("master_service must retain an HTTP/HTTPS tunnel listener")
	}
	return strings.Join(http, ","), nil
}
