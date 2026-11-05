package vpn

import (
	"errors"
	"strconv"
)

func recoverClientRoute(s clientRecoveryState) error {
	if !s.Endpoint.Is4() || !s.Gateway.Is4() || s.Index <= 0 {
		return errors.New("invalid VPN endpoint route record")
	}
	pin := " -DestinationPrefix " + psQuote(s.Endpoint.String()+"/32") + " -InterfaceIndex " + strconv.Itoa(s.Index) + " -NextHop " + psQuote(s.Gateway.String()) + " -PolicyStore ActiveStore"
	_, err := runWindowsNetworkCommand("powershell.exe", powershellArgs("if (Get-NetRoute"+pin+" -ErrorAction SilentlyContinue) { Remove-NetRoute"+pin+" -Confirm:$false }")...)
	return err
}

func recoverClientDNS(clientRecoveryState) error {
	return errors.New("unexpected VPN DNS recovery record")
}
