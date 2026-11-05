package vpn

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
)

func recoverClientRoute(s clientRecoveryState) error {
	if !s.Endpoint.Is4() || !s.Gateway.Is4() || s.Device == "" || interfaceUnsafe(s.Device) {
		return errors.New("invalid VPN endpoint route record")
	}
	out, err := runCommand("route", "-n", "get", s.Endpoint.String())
	if err != nil {
		return err
	}
	gateway, device, destination := macRouteDetails(out)
	if gateway != s.Gateway.String() || device != s.Device || destination != s.Endpoint.String() {
		return nil
	}
	_, err = runCommand("route", "-n", "delete", "-host", s.Endpoint.String(), s.Gateway.String())
	return err
}

func recoverClientDNS(s clientRecoveryState) error {
	if len(s.VPNDNS) == 0 || len(s.PreviousDNS) == 0 || strings.HasPrefix(s.DNSService, "-") {
		return errors.New("invalid VPN DNS recovery record")
	}
	for _, addr := range append(slices.Clone(s.VPNDNS), s.PreviousDNS...) {
		if _, err := netip.ParseAddr(addr); err != nil && !(len(s.PreviousDNS) == 1 && s.PreviousDNS[0] == "Empty" && addr == "Empty") {
			return err
		}
	}
	out, err := runCommand("networksetup", "-getdnsservers", s.DNSService)
	if err != nil {
		return err
	}
	if !slices.Equal(strings.Fields(out), s.VPNDNS) {
		return nil
	}
	_, err = runCommand("networksetup", append([]string{"-setdnsservers", s.DNSService}, s.PreviousDNS...)...)
	return err
}
