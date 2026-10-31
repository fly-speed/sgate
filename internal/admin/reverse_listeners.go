package admin

import (
	"errors"
	"net"
	"strings"

	"sgate/internal/tunnel"
)

// SyncReverseListeners commits framework sockets together with the account
// update. apply runs after the .cf write and a failure restores its original.
func SyncReverseListeners(path string, previous []TunnelAccountInfo, accounts []tunnel.Account,
	apply func(string, bool) error) (string, bool, error) {
	original, err := regularRead(path, 1<<20)
	if err != nil {
		return "", false, errors.New("无法读取服务端监听配置")
	}
	fields, err := configFields(string(original))
	if err != nil {
		return "", false, err
	}
	var current string
	var configuredListen string
	exists := false
	for _, field := range fields {
		if field.Key == "listen" {
			configuredListen = field.Value
		}
		if field.Key == "master_service" {
			current, exists = field.Value, true
		}
	}
	if err := ValidateReverseTunnelPorts(accounts, configuredListen); err != nil {
		return "", false, err
	}
	if !exists {
		return "", false, apply("", false)
	}
	old := map[string]bool{}
	for _, account := range previous {
		if account.Disabled {
			continue
		}
		for _, mapping := range account.ReverseMappings {
			old[mapping.Listen] = true
		}
	}
	var endpoints []string
	base := map[string]bool{}
	byPort := map[string][]string{}
	addBase := func(address string) {
		base[address] = true
		_, port, _ := net.SplitHostPort(address)
		byPort[port] = append(byPort[port], address)
	}
	for _, endpoint := range strings.FieldsFunc(current, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' }) {
		address := strings.ReplaceAll(endpoint, "|", ":")
		if !old[address] {
			endpoints = append(endpoints, endpoint)
			addBase(address)
		}
	}
	if len(endpoints) == 0 {
		return "", false, errors.New("服务端必须保留 HTTP 隧道监听地址")
	}
	if err := ValidateReverseTunnelPorts(accounts, strings.Join(endpoints, ",")); err != nil {
		return "", false, err
	}
	for _, account := range accounts {
		if account.Disabled {
			continue
		}
		for _, mapping := range account.ReverseMappings {
			_, listenPort, _ := net.SplitHostPort(mapping.Listen)
			conflict := base[mapping.Listen]
			for _, address := range byPort[listenPort] {
				if reverseListenerOverlap(address, mapping.Listen) {
					conflict = true
				}
			}
			if conflict {
				return "", false, errors.New("反向代理监听地址与现有监听冲突")
			}
			host, port, err := net.SplitHostPort(mapping.Listen)
			if err != nil {
				return "", false, err
			}
			endpoint := net.JoinHostPort(host, port)
			at := strings.LastIndexByte(endpoint, ':')
			endpoints = append(endpoints, endpoint[:at]+"|"+endpoint[at+1:])
			addBase(mapping.Listen)
		}
	}
	next := strings.Join(endpoints, ", ")
	changed := !sameMappingListeners(current, next)
	if !changed {
		return next, false, apply(next, false)
	}
	updated, err := updateConfigFields(original, map[string]string{"master_service": next})
	if err != nil {
		return "", false, err
	}
	err = persistTunnelAccounts(path, original, []byte(updated), func() error { return apply(next, true) })
	return next, err == nil, err
}

func reverseListenerOverlap(a, b string) bool {
	ah, ap, ae := net.SplitHostPort(a)
	bh, bp, be := net.SplitHostPort(b)
	if ae != nil || be != nil || ap != bp {
		return false
	}
	ai, bi := net.ParseIP(ah), net.ParseIP(bh)
	return ah == "" || bh == "" || ah == bh || (ai != nil && bi != nil && (ai.Equal(bi) || ai.IsUnspecified() || bi.IsUnspecified()))
}

// Compare port numbers regardless of bind IP: raw TCP mappings and WebSocket
// listeners must have separate service ports, including disabled mappings.
var ErrReverseTunnelPortConflict = errors.New("反向代理监听端口不能与 WebSocket 隧道服务端口相同")

func ValidateReverseTunnelPorts(accounts []tunnel.Account, listeners ...string) error {
	ports := map[int]bool{}
	for _, raw := range listeners {
		for _, address := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' }) {
			_, port, err := net.SplitHostPort(strings.ReplaceAll(address, "|", ":"))
			number, portErr := net.LookupPort("tcp", port)
			if err == nil && portErr == nil && number > 0 {
				ports[number] = true
			}
		}
	}
	for _, account := range accounts {
		for _, mapping := range account.ReverseMappings {
			_, port, err := net.SplitHostPort(mapping.Listen)
			number, portErr := net.LookupPort("tcp", port)
			if err == nil && portErr == nil && ports[number] {
				return ErrReverseTunnelPortConflict
			}
		}
	}
	return nil
}
