// Rootless, route-free VPN diagnostic. The optional netstack dependency lives
// in this separate module and is not linked into sgate production binaries.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	neturl "net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/html"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"
	"sgate/internal/serviceconfig"
	"sgate/internal/vpn"
)

type device struct {
	tun.Device
	once    sync.Once
	err     error
	packets *packetSummary
}

func (d *device) Read(p []byte) (int, error) {
	sizes := []int{0}
	_, err := d.Device.Read([][]byte{p}, sizes, 0)
	return sizes[0], err
}
func (d *device) Write(p []byte) (int, error) {
	if d.packets != nil {
		d.packets.record(p)
	}
	n, err := d.Device.Write([][]byte{p}, 0)
	if n == 1 {
		return len(p), err
	}
	return 0, err
}
func (d *device) Close() error { d.once.Do(func() { d.err = d.Device.Close() }); return d.err }
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "", "existing integrated client .cf")
	serverOverride := flag.String("server", "", "optional VPN endpoint override for transport comparison; does not save configuration")
	output := flag.String("output", "", "new private JSON report")
	dnsSource := flag.String("dns-source", "system", "system or vpn DNS for target addresses")
	httpMode := flag.String("http", "h2", "h1, h2 or h3 (h3 never falls back)")
	reference := flag.String("reference", "", "pin target IPv4 addresses from an earlier rootless report")
	only := flag.String("only", "", "optional single host from the diagnostic allowlist")
	assets := flag.Bool("assets", false, "also fetch up to six actual script/stylesheet URLs from public login HTML")
	proxyAddress := flag.String("proxy-listen", "127.0.0.1:0", "loopback IPv4 proxy address")
	proxy := flag.Bool("browser-proxy", false, "serve a temporary loopback CONNECT proxy for Google domains; no system changes")
	assetURL := flag.String("asset-url", "", "single public HTTPS script URL from an observed browser request; no credentials")
	timeout := flag.Duration("request-timeout", 45*time.Second, "per-request limit")
	budget := flag.Duration("budget", 180*time.Second, "overall deadline")
	flag.Parse()
	if *path == "" || *output == "" || (*dnsSource != "system" && *dnsSource != "vpn") || (*httpMode != "h1" && *httpMode != "h2" && *httpMode != "h3") {
		return errors.New("require config, output, dns-source=system|vpn, http=h1|h2|h3")
	}
	values, merged, err := serviceconfig.Read(*path)
	if err != nil {
		return err
	}
	if !merged {
		return errors.New("diagnostic expects an integrated service .cf")
	}
	fs := flag.NewFlagSet("client-config", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	vpn.RegisterFlags(fs, "client")
	fs.String("key-file", "", "")
	fs.String("tls-ca", "", "")
	fs.VisitAll(func(f *flag.Flag) {
		if v, ok := values[f.Name]; ok {
			if err == nil {
				err = fs.Set(f.Name, v)
			}
		}
	})
	if err != nil {
		return errors.New("invalid VPN configuration value")
	}
	if *serverOverride != "" {
		fs.Set("vpn-server", *serverOverride)
	}
	fs.Set("vpn-mode", "only")
	p, err := vpn.Prepare("client", fs)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, deadline := context.WithTimeout(ctx, *budget)
	defer deadline()
	config, network, err := p.Resolve(ctx)
	if err != nil {
		return err
	}
	report := map[string]any{"scope": "rootless userspace IPv4/TCP; no system routes, DNS, TUN or browser cookies", "dns_source": *dnsSource, "requested_http": *httpMode, "started_utc": time.Now().UTC().Format(time.RFC3339)}
	file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	save := func() {
		file.Seek(0, 0)
		json.NewEncoder(file).Encode(report)
		position, _ := file.Seek(0, 1)
		file.Truncate(position)
		file.Sync()
	}
	probe := vpn.Probe(ctx, config)
	report["probe"] = probe
	save()
	if !probe.OK {
		return errors.New("VPN probe failed; see report")
	}
	if probe.AddressInUse || probe.ClientLimitReached {
		return errors.New("VPN address occupied or server full; existing connections were not changed")
	}
	// A probe suggests a free address but never reserves it. The real
	// authenticated session claims it atomically or fails without eviction.
	config.ClientIP = netip.MustParseAddr(probe.ClientIP)
	p.Config.ClientIP = config.ClientIP
	dns := network.DNS
	if len(dns) == 0 {
		dns = []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	}
	raw, stack, err := netstack.CreateNetTUN([]netip.Addr{config.ClientIP}, dns, config.MTU)
	if err != nil {
		return err
	}
	packets := &packetSummary{}
	dev := &device{Device: raw, packets: packets}
	defer dev.Close()
	stats := &vpn.Stats{}
	config.Stats = stats
	config.Noise = vpn.NoiseConfig{}
	config.Connected = nil
	config.Logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- vpn.RunClient(runCtx, config, dev) }()
	defer func() {
		stop()
		dev.Close()
		<-done
		report["stats"] = stats.Snapshot()
		report["inbound_packets"] = packets.snapshot()
		save()
	}()
	for stats.Snapshot().Active == 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			done <- err
			return fmt.Errorf("VPN ended: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if *proxy {
		report["scope"] = "browser CONNECT over rootless VPN; end-to-end TLS; no system routes or DNS changes; no cookies, URLs or HTTP bodies logged"
		var lookup proxyLookup = stack.LookupContextHost
		if *dnsSource == "system" {
			lookup = net.DefaultResolver.LookupHost
		}
		return serveBrowserProxy(ctx, *proxyAddress, resolvedProxyDial(stack.DialContext, lookup), report, save)
	}
	targets := []string{"mail.google.com", "accounts.google.com", "www.gstatic.com", "ssl.gstatic.com"}
	addresses := map[string]string{}
	for _, host := range targets {
		lookupCtx, stopLookup := context.WithTimeout(ctx, 8*time.Second)
		var answers []string
		if *dnsSource == "vpn" {
			answers, err = stack.LookupContextHost(lookupCtx, host)
		} else {
			answers, err = net.DefaultResolver.LookupHost(lookupCtx, host)
		}
		stopLookup()
		if err == nil {
			for _, answer := range answers {
				ip, e := netip.ParseAddr(answer)
				if e == nil && ip.Is4() {
					addresses[host] = ip.String()
					break
				}
			}
		}
		if addresses[host] == "" {
			report["error"] = "DNS failed for " + host
			save()
			return errors.New("target DNS failed")
		}
	}
	if *reference != "" {
		data, e := os.ReadFile(*reference)
		if e != nil {
			return e
		}
		var prior struct {
			IPs map[string]string `json:"target_ips"`
		}
		if e = json.Unmarshal(data, &prior); e != nil {
			return e
		}
		for _, host := range targets {
			ip, e := netip.ParseAddr(prior.IPs[host])
			if e != nil || !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
				return errors.New("reference has invalid target IPv4")
			}
			addresses[host] = ip.String()
		}
	}
	report["target_ips"] = addresses
	save()
	transport := &http.Transport{Proxy: nil, ForceAttemptHTTP2: *httpMode == "h2", DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		ip, ok := addresses[host]
		if !ok {
			return nil, errors.New("target outside diagnostic allowlist")
		}
		return stack.DialContext(ctx, "tcp4", net.JoinHostPort(ip, port))
	}}
	if *httpMode == "h1" {
		transport.ForceAttemptHTTP2 = false
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	defer transport.CloseIdleConnections()
	var roundTripper http.RoundTripper = transport
	if *httpMode == "h3" {
		h3 := &http3.Transport{QUICConfig: &quic.Config{HandshakeIdleTimeout: 10 * time.Second, MaxIdleTimeout: 20 * time.Second, InitialPacketSize: 1200, DisablePathMTUDiscovery: true}, Dial: func(ctx context.Context, address string, tlsConfig *tls.Config, config *quic.Config) (*quic.Conn, error) {
			host, port, e := net.SplitHostPort(address)
			if e != nil {
				return nil, e
			}
			ip, ok := addresses[host]
			if !ok {
				return nil, errors.New("HTTP3 target outside allowlist")
			}
			destination, e := net.ResolveUDPAddr("udp4", net.JoinHostPort(ip, port))
			if e != nil {
				return nil, e
			}
			packet, e := stack.ListenUDP(&net.UDPAddr{IP: p.Config.ClientIP.AsSlice()})
			if e != nil {
				return nil, e
			}
			tr := &quic.Transport{Conn: packet}
			conn, e := tr.Dial(ctx, destination, tlsConfig, config)
			if e != nil {
				tr.Close()
				packet.Close()
				return nil, e
			}
			go func() { <-conn.Context().Done(); tr.Close(); packet.Close() }()
			return conn, nil
		}}
		defer h3.Close()
		roundTripper = h3
	}
	client := &http.Client{Transport: roundTripper, Timeout: *timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != "https" || addresses[req.URL.Hostname()] == "" {
			return http.ErrUseLastResponse
		}
		return nil
	}}

	var results []map[string]any
	urls := []string{"https://mail.google.com/mail/u/0/", "https://accounts.google.com/", "https://www.gstatic.com/", "https://ssl.gstatic.com/"}
	if *assetURL != "" {
		u, e := neturl.Parse(*assetURL)
		if e != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || addresses[u.Hostname()] == "" || (u.Port() != "" && u.Port() != "443") || !strings.HasPrefix(u.Path, "/_/scs/mail-static/") {
			return errors.New("asset-url requires an allowlisted public mail-static HTTPS URL without credentials or query parameters")
		}
		urls = []string{u.String()}
	}
	if *only != "" {
		if addresses[*only] == "" {
			return errors.New("only host outside allowlist")
		}
		target := "https://" + *only + "/"
		if *only == "mail.google.com" {
			target += "mail/u/0/"
		}
		urls = []string{target}
	}
	for index := 0; index < len(urls) && index < 10; index++ {
		target := urls[index]
		parsed, _ := neturl.Parse(target)
		host := parsed.Hostname()
		started := time.Now()
		before := stats.Snapshot()
		entry := map[string]any{"host": host, "target_ip": addresses[host], "kind": "public_page"}
		if index >= 4 {
			entry["kind"] = "script_or_stylesheet"
			entry["asset_index"] = index - 3
		}
		var traceMu sync.Mutex
		var local, remote string
		var firstByte int64
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
			traceMu.Lock()
			local = info.Conn.LocalAddr().String()
			remote = info.Conn.RemoteAddr().String()
			traceMu.Unlock()
		}, GotFirstResponseByte: func() { traceMu.Lock(); firstByte = time.Since(started).Milliseconds(); traceMu.Unlock() }}
		req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), "GET", target, nil)
		response, e := client.Do(req)
		if response != nil {
			entry["status"] = response.StatusCode
			entry["protocol"] = response.Proto
			entry["body_gzip_decoded"] = response.Uncompressed
			entry["content_type"] = response.Header.Get("Content-Type")
			entry["alt_svc"] = response.Header.Get("Alt-Svc")
			body, readErr := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
			response.Body.Close()
			e = readErr
			entry["body_bytes"] = len(body)
			entry["complete"] = readErr == nil && len(body) <= 4<<20
			if len(body) > 4<<20 {
				entry["sample_limit"] = true
			}
			if *assets && index == 0 && readErr == nil {
				found := collectAssets(body, response.Request.URL, addresses)
				report["sampled_assets"] = len(found)
				urls = append(urls, found...)
			}
		}
		if e != nil {
			var uerr *neturl.Error
			if errors.As(e, &uerr) {
				entry["error"] = uerr.Err.Error()
			} else {
				entry["error"] = e.Error()
			}
		}
		traceMu.Lock()
		entry["local"] = local
		entry["remote"] = remote
		entry["first_byte_ms"] = firstByte
		traceMu.Unlock()
		entry["elapsed_ms"] = time.Since(started).Milliseconds()
		after := stats.Snapshot()
		entry["vpn_rx_ip_bytes"] = after.RX - before.RX
		entry["vpn_tx_ip_bytes"] = after.TX - before.TX
		results = append(results, entry)
		report["https"] = results
		report["stats"] = after
		save()
		fmt.Fprintf(os.Stderr, "%s %s HTTP=%v bytes=%v complete=%v elapsed=%vms error=%v\n", entry["kind"], host, entry["status"], entry["body_bytes"], entry["complete"], entry["elapsed_ms"], entry["error"])
	}

	report["finished"] = true
	return nil
}

// Only extract URLs; never execute page scripts or retain HTML/cookies.
func collectAssets(body []byte, base *neturl.URL, allowed map[string]string) []string {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var result []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(result) >= 6 {
			return
		}
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "link") {
			attrs := map[string]string{}
			for _, a := range n.Attr {
				attrs[a.Key] = a.Val
			}
			raw := attrs["src"]
			if n.Data == "link" {
				if attrs["rel"] == "stylesheet" {
					raw = attrs["href"]
				} else {
					raw = ""
				}
			}
			if u, err := neturl.Parse(raw); err == nil && raw != "" {
				u = base.ResolveReference(u)
				u.Fragment = ""
				if u.Scheme == "https" && u.User == nil && (u.Port() == "" || u.Port() == "443") && allowed[u.Hostname()] != "" && !seen[u.String()] {
					seen[u.String()] = true
					result = append(result, u.String())
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return result
}

type packetSummary struct {
	mu             sync.Mutex
	TCP, UDP, ICMP int
	Errors         []map[string]any
}

func (s *packetSummary) record(p []byte) {
	if len(p) < 20 || p[0]>>4 != 4 {
		return
	}
	ihl := int(p[0]&15) * 4
	if ihl < 20 || ihl > len(p) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch p[9] {
	case 6:
		s.TCP++
	case 17:
		s.UDP++
	case 1:
		s.ICMP++
		if len(p) >= ihl+8 && (p[ihl] == 3 || p[ihl] == 11 || p[ihl] == 12) && len(s.Errors) < 16 {
			src := netip.AddrFrom4([4]byte(p[12:16]))
			entry := map[string]any{"source": src.String(), "type": p[ihl], "code": p[ihl+1]}
			if p[ihl] == 3 && p[ihl+1] == 4 {
				entry["next_hop_mtu"] = binary.BigEndian.Uint16(p[ihl+6 : ihl+8])
			}
			quoted := p[ihl+8:]
			if len(quoted) >= 20 && quoted[0]>>4 == 4 {
				entry["quoted_destination"] = netip.AddrFrom4([4]byte(quoted[16:20])).String()
				entry["quoted_protocol"] = quoted[9]
			}
			s.Errors = append(s.Errors, entry)
		}
	}
}
func (s *packetSummary) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"tcp": s.TCP, "udp": s.UDP, "icmp": s.ICMP, "errors": append([]map[string]any(nil), s.Errors...)}
}
