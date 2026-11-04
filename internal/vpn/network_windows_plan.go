package vpn

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"unicode/utf16"
)

// The plan is platform independent so rollback and quoting can be verified
// without changing the test host's routing table.
func powershellArgs(script string) []string {
	words := utf16.Encode([]rune("$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'; [Console]::OutputEncoding=New-Object System.Text.UTF8Encoding; " + script))
	b := make([]byte, len(words)*2)
	for i, w := range words {
		binary.LittleEndian.PutUint16(b[i*2:], w)
	}
	return []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(b)}
}
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func applyWindowsNetwork(o NetworkOptions, run commandRunner) (cleanup func(), err error) {
	if o.Server {
		return nil, errors.New("Windows VPN server network management is not supported")
	}
	if o.Interface == "" || !validClientIP(o.Network, o.ClientIP) || !o.Endpoint.Is4() || o.MTU < 576 || o.MTU > 1500 {
		return nil, errors.New("invalid Windows VPN network settings")
	}
	t := &networkTransaction{run: run, logf: o.Logf}
	defer func() {
		if err != nil {
			t.close()
		}
	}()
	ps := func(script string) (string, error) { return run("powershell.exe", powershellArgs(script)...) }
	step := func(script, undo string) error {
		var reverse func() error
		if undo != "" {
			reverse = func() error { _, e := ps(undo); return e }
		}
		return t.step("powershell.exe", powershellArgs(script), reverse)
	}
	// Resolve the physical route before adding any tunnel routes.
	var route struct {
		InterfaceIndex          int
		InterfaceAlias, NextHop string
	}
	output, e := ps("Find-NetRoute -RemoteIPAddress " + psQuote(o.Endpoint.String()) + " | Where-Object { $null -ne $_.NextHop } | Select-Object -First 1 InterfaceIndex,InterfaceAlias,NextHop | ConvertTo-Json -Compress")
	if e != nil {
		return nil, e
	}
	if json.Unmarshal([]byte(strings.TrimPrefix(output, "\ufeff")), &route) != nil || route.InterfaceIndex <= 0 || route.InterfaceAlias == o.Interface || interfaceUnsafe(route.InterfaceAlias) {
		return nil, errors.New("cannot determine a physical Windows route to the VPN endpoint")
	}
	next, e := netip.ParseAddr(route.NextHop)
	if e != nil || !next.Is4() {
		return nil, errors.New("invalid Windows endpoint gateway")
	}
	index := strconv.Itoa(route.InterfaceIndex)
	pin := " -DestinationPrefix " + psQuote(o.Endpoint.String()+"/32") + " -InterfaceIndex " + index + " -NextHop " + psQuote(next.String())
	output, e = ps("if (Get-NetRoute" + pin + " -PolicyStore ActiveStore -ErrorAction SilentlyContinue) { 'existing' } else { New-NetRoute" + pin + " -PolicyStore ActiveStore -RouteMetric 1 | Out-Null; 'created' }")
	if e != nil {
		return nil, e
	}
	if strings.TrimSpace(output) == "created" {
		t.undo = append(t.undo, func() error {
			_, e := ps("Remove-NetRoute" + pin + " -PolicyStore ActiveStore -Confirm:$false")
			return e
		})
	} else if strings.TrimSpace(output) != "existing" {
		return nil, errors.New("unexpected endpoint route response")
	}
	output, e = ps("$a=@(Get-NetAdapter -IncludeHidden | Where-Object { $_.Name -eq " + psQuote(o.Interface) + " }); if ($a.Count -ne 1) { throw 'VPN adapter not found or ambiguous' }; $a[0].ifIndex")
	if e != nil {
		return nil, e
	}
	tunIndex, e := strconv.Atoi(strings.TrimSpace(output))
	if e != nil || tunIndex <= 0 || tunIndex == route.InterfaceIndex {
		return nil, errors.New("invalid Windows VPN adapter index")
	}
	iface := " -InterfaceIndex " + strconv.Itoa(tunIndex)
	if err = step("Set-NetIPInterface"+iface+" -AddressFamily IPv4 -Dhcp Disabled -NlMtuBytes "+strconv.Itoa(o.MTU)+" -AutomaticMetric Disabled -InterfaceMetric 5", ""); err != nil {
		return nil, err
	}
	address := iface + " -IPAddress " + psQuote(o.ClientIP.String())
	if err = step("New-NetIPAddress"+address+" -PrefixLength 32 -AddressFamily IPv4 -PolicyStore ActiveStore | Out-Null", "Remove-NetIPAddress"+address+" -Confirm:$false"); err != nil {
		return nil, err
	}
	prefixes := append([]netip.Prefix{o.Network}, o.Routes...)
	if o.BlockIPv6 {
		prefixes = append(prefixes, netip.MustParsePrefix("::/1"), netip.MustParsePrefix("8000::/1"))
	}
	seen := map[netip.Prefix]bool{}
	for _, prefix := range prefixes {
		if !prefix.IsValid() || (!prefix.Addr().Is4() && !(o.BlockIPv6 && prefix.Addr().Is6())) {
			return nil, errors.New("invalid Windows VPN route")
		}
		prefix = prefix.Masked()
		if seen[prefix] {
			continue
		}
		seen[prefix] = true
		hop := "0.0.0.0"
		if prefix.Addr().Is6() {
			hop = "::"
		}
		args := iface + " -DestinationPrefix " + psQuote(prefix.String()) + " -NextHop " + psQuote(hop) + " -PolicyStore ActiveStore"
		if err = step("New-NetRoute"+args+" -RouteMetric 1 | Out-Null", "Remove-NetRoute"+args+" -Confirm:$false"); err != nil {
			return nil, err
		}
	}
	if len(o.DNS) > 0 {
		dns := []string{}
		for _, ip := range o.DNS {
			if !ip.Is4() {
				return nil, fmt.Errorf("Windows VPN DNS requires IPv4")
			}
			dns = append(dns, psQuote(ip.String()))
		}
		if err = step("Set-DnsClientServerAddress"+iface+" -ServerAddresses @("+strings.Join(dns, ",")+")", "Set-DnsClientServerAddress"+iface+" -ResetServerAddresses"); err != nil {
			return nil, err
		}
	}
	return t.close, nil
}
