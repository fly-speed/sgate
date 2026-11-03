//go:build darwin

package vpn

import (
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func macServerOptions() NetworkOptions {
	return NetworkOptions{Server: true, Interface: "utun99", Network: netip.MustParsePrefix("10.77.0.0/24"), MTU: 1280}
}
func TestMacServerPFSetupAndCleanup(t *testing.T) {
	for _, previous := range []string{"0", "1"} {
		for _, failure := range []string{"", "syntax", "route", "load", "forward"} {
			t.Run(previous+"-"+failure, func(t *testing.T) {
				var calls []string
				var rules, path, anchor string
				run := func(name string, args ...string) (string, error) {
					line := name + " " + strings.Join(args, " ")
					calls = append(calls, line)
					switch line {
					case "route -n get 1.1.1.1":
						return "gateway: 192.168.1.1\ninterface: en0", nil
					case "pfctl -s nat":
						return "nat-anchor \"com.apple/*\" all", nil
					case "pfctl -s rules":
						return "anchor \"com.apple/*\" all", nil
					case "sysctl -n net.inet.ip.forwarding":
						return previous, nil
					case "pfctl -E":
						return "pf enabled\nToken : 12345678", nil
					}
					if name == "pfctl" && len(args) > 0 && args[0] == "-n" {
						anchor = args[2]
						path = args[4]
						b, e := os.ReadFile(path)
						if e != nil {
							t.Fatal(e)
						}
						rules = string(b)
						info, _ := os.Stat(path)
						if info.Mode().Perm() != 0600 {
							t.Fatal("PF file permissions")
						}
						if failure == "syntax" {
							return "", errors.New("syntax failure")
						}
					}
					if failure == "route" && strings.HasPrefix(line, "route -n add") {
						return "", errors.New("route conflict")
					}
					if failure == "load" && name == "pfctl" && len(args) == 4 && args[0] == "-a" && args[2] == "-f" {
						return "", errors.New("load failure")
					}
					if failure == "forward" && line == "sysctl -w net.inet.ip.forwarding=1" {
						return "", errors.New("forward failure")
					}
					return "", nil
				}
				undo, e := applyNetwork(macServerOptions(), run)
				wantError := failure != "" && !(failure == "forward" && previous == "1")
				if (e != nil) != wantError {
					t.Fatal(e)
				}
				if e == nil {
					undo()
					undo()
				}
				if _, e = os.Stat(path); !os.IsNotExist(e) {
					t.Fatal("PF temporary file retained", path, e)
				}
				joined := strings.Join(calls, "\n")
				for _, forbidden := range []string{"pfctl -d", "pfctl -f /etc/pf.conf", "pfctl -F ", "pfctl -a com.apple -", " -F states", " -F all"} {
					if strings.Contains(joined, forbidden) {
						t.Fatal("modified shared PF state", joined)
					}
				}
				if !strings.HasPrefix(anchor, "com.apple/sgate-vpn-") {
					t.Fatal(anchor)
				}
				if !strings.Contains(rules, "nat on en0 inet from 10.77.0.0/24 to any tag SGATEVPN") || !strings.Contains(rules, " -> (en0)") || !strings.Contains(rules, "inet all tagged SGATEVPN") || !strings.Contains(rules, "block drop quick on utun99 all") {
					t.Fatal(rules)
				}
				if failure != "syntax" && failure != "route" {
					for _, required := range []string{"pfctl -X 12345678", "pfctl -a " + anchor + " -F nat", "pfctl -a " + anchor + " -F rules", "route -n delete -net 10.77.0.0/24 -interface utun99"} {
						if !strings.Contains(joined, required) {
							t.Fatal(required, joined)
						}
					}
				}
				if previous == "1" && strings.Contains(joined, "sysctl -w") {
					t.Fatal("changed pre-existing forwarding")
				}
				if previous == "0" && failure == "" && !strings.Contains(joined, "sysctl -w net.inet.ip.forwarding=0") {
					t.Fatal("forwarding not restored")
				}
			})
		}
	}
}
func TestMacServerRejectsUnsafeEgressAndMissingPFHooksBeforeChanges(t *testing.T) {
	for _, egress := range []string{"utun2", "lo0", "en0\npass all", "en0"} {
		var calls []string
		run := func(name string, args ...string) (string, error) {
			line := name + " " + strings.Join(args, " ")
			calls = append(calls, line)
			return "", nil
		}
		o := macServerOptions()
		o.Egress = egress
		if _, e := applyNetwork(o, run); e == nil {
			t.Fatal("invalid egress/hooks accepted", egress)
		}
		for _, line := range calls {
			if strings.Contains(line, " -w ") || strings.Contains(line, " -E") || strings.Contains(line, " add ") || strings.Contains(line, " inet ") {
				t.Fatal("mutated host before preflight", line)
			}
		}
	}
}
func TestMacServerDoesNotDisableSharedPFWhenTokenMissing(t *testing.T) {
	calls := []string{}
	run := func(name string, args ...string) (string, error) {
		line := name + " " + strings.Join(args, " ")
		calls = append(calls, line)
		switch line {
		case "route -n get 1.1.1.1":
			return "interface: en0", nil
		case "pfctl -s nat":
			return `nat-anchor "com.apple/*" all`, nil
		case "pfctl -s rules":
			return `anchor "com.apple/*" all`, nil
		case "sysctl -n net.inet.ip.forwarding":
			return "0", nil
		case "pfctl -E":
			return "no token", nil
		}
		return "", nil
	}
	_, e := applyNetwork(macServerOptions(), run)
	if e == nil || !strings.Contains(e.Error(), "no reference token") {
		t.Fatal(e)
	}
	for _, line := range calls {
		if line == "pfctl -d" || strings.HasPrefix(line, "pfctl -X ") {
			t.Fatal("guessed PF enable token", calls)
		}
	}
}

// pfctl -n only parses; this test never loads rules or enables PF.
func TestMacServerGeneratedRulesPassSystemPFSyntaxCheck(t *testing.T) {
	o := macServerOptions()
	o.Interface = "lo0"
	path := t.TempDir() + "/vpn-pf.conf"
	if e := os.WriteFile(path, []byte(macServerPFRules(o, "lo0", "SGATEVPNTEST")), 0600); e != nil {
		t.Fatal(e)
	}
	output, e := exec.Command("/sbin/pfctl", "-n", "-a", "com.apple/sgate-vpn-syntax-test", "-f", path).CombinedOutput()
	if e != nil {
		t.Fatalf("PF syntax: %v: %s", e, output)
	}
}
