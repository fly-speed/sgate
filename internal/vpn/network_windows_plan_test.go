package vpn

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"unicode/utf16"
)

func decodedPowerShell(t *testing.T, args []string) string {
	t.Helper()
	b, e := base64.StdEncoding.DecodeString(args[len(args)-1])
	if e != nil {
		t.Fatal(e)
	}
	words := make([]uint16, len(b)/2)
	for i := range words {
		words[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(words))
}
func windowsNetworkFixture() NetworkOptions {
	return NetworkOptions{Interface: "sgate VPN 'test'", Network: netip.MustParsePrefix("10.77.0.0/24"), ClientIP: netip.MustParseAddr("10.77.0.2"), Endpoint: netip.MustParseAddr("192.0.2.1"), MTU: 1280, Routes: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/1"), netip.MustParsePrefix("128.0.0.0/1")}, DNS: []netip.Addr{netip.MustParseAddr("1.1.1.1")}, BlockIPv6: true}
}
func TestWindowsNetworkRollbackAndExistingEndpointRoute(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			var commands []string
			run := func(name string, args ...string) (string, error) {
				if name != "powershell.exe" {
					t.Fatal(name)
				}
				script := decodedPowerShell(t, args)
				commands = append(commands, script)
				switch {
				case strings.Contains(script, "Find-NetRoute"):
					return `{"InterfaceIndex":4,"InterfaceAlias":"Ethernet","NextHop":"192.168.1.1"}`, nil
				case strings.Contains(script, "if (Get-NetRoute"):
					if existing {
						return "existing", nil
					}
					return "created", nil
				case strings.Contains(script, "Get-NetAdapter"):
					return "19", nil
				case fail && strings.Contains(script, "-ServerAddresses"):
					return "", errors.New("injected DNS failure")
				}
				return "", nil
			}
			undo, err := applyWindowsNetwork(windowsNetworkFixture(), run)
			if fail {
				if err == nil {
					t.Fatal("failure ignored")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				undo()
			}
			joined := strings.Join(commands, "\n")
			if !strings.Contains(joined, "'sgate VPN ''test''' ") || !strings.Contains(joined, "Remove-NetIPAddress -InterfaceIndex 19") || !strings.Contains(joined, "Remove-NetRoute -InterfaceIndex 19") {
				t.Fatal("missing quoting or rollback", joined)
			}
			removedPin := strings.Contains(joined, "Remove-NetRoute -DestinationPrefix '192.0.2.1/32'")
			if removedPin == existing {
				t.Fatal("endpoint route ownership was not preserved")
			}
			if !strings.Contains(joined, "'::/1'") || !strings.Contains(joined, "'8000::/1'") {
				t.Fatal("IPv6 block routes missing")
			}
			if strings.Contains(joined, "Set-DnsClientServerAddress -InterfaceIndex 4") || strings.Contains(joined, "Remove-NetIPAddress -InterfaceIndex 4") {
				t.Fatal("physical interface was changed")
			}
		}
	}
}
func TestWindowsNetworkRejectsRecursiveEndpointBeforeMutation(t *testing.T) {
	run := func(_ string, args ...string) (string, error) {
		s := decodedPowerShell(t, args)
		if !strings.Contains(s, "Find-NetRoute") {
			t.Fatal("mutated before rejecting recursive route")
		}
		return `{"InterfaceIndex":19,"InterfaceAlias":"sgateVPN","NextHop":"0.0.0.0"}`, nil
	}
	if _, err := applyWindowsNetwork(windowsNetworkFixture(), run); err == nil {
		t.Fatal("recursive endpoint accepted")
	}
}
