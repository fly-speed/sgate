package vpn

import (
	"errors"
	"slices"
	"strings"
)

func recoverClientRoute(s clientRecoveryState) error {
	if !s.Endpoint.Is4() || (s.Gateway.IsValid() && !s.Gateway.Is4()) || s.Device == "" || interfaceUnsafe(s.Device) || strings.HasPrefix(s.Device, "-") {
		return errors.New("invalid VPN endpoint route record")
	}
	prefix := s.Endpoint.String() + "/32"
	out, err := runCommand("ip", "-4", "route", "show", "exact", prefix)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		device, gateway := slices.Index(fields, "dev"), slices.Index(fields, "via")
		if device < 0 || device+1 >= len(fields) || fields[device+1] != s.Device {
			continue
		}
		args := []string{"-4", "route", "del", prefix}
		if s.Gateway.IsValid() {
			if gateway < 0 || gateway+1 >= len(fields) || fields[gateway+1] != s.Gateway.String() {
				continue
			}
			args = append(args, "via", s.Gateway.String())
		} else if gateway >= 0 {
			continue
		}
		_, err = runCommand("ip", append(args, "dev", s.Device)...)
		return err
	}
	return nil
}

func recoverClientDNS(clientRecoveryState) error {
	return errors.New("unexpected VPN DNS recovery record")
}
