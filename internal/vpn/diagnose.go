package vpn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"sort"
	"sync"
	"time"
)

// Diagnostic runs a real IP/TCP/TLS transfer using a fresh TUN, without
// replacing default routes or DNS. An existing lease is never displaced.
type Diagnostic struct {
	Probe   ProbeResult      `json:"probe"`
	Stage   string           `json:"stage"`
	Error   string           `json:"error,omitempty"`
	Device  string           `json:"device,omitempty"`
	Routes  []string         `json:"test_routes,omitempty"`
	Results []HTTPDiagnostic `json:"https,omitempty"`
	Stats   Snapshot         `json:"stats"`
}
type HTTPDiagnostic struct {
	LocalAddress  string `json:"local_address,omitempty"`
	RemoteAddress string `json:"remote_address,omitempty"`
	FirstByteMS   int64  `json:"first_byte_ms"`
	Uncompressed  bool   `json:"body_was_gzip_decoded"`
	IPTXBytes     uint64 `json:"vpn_tx_ip_bytes_during_request"`
	IPRXBytes     uint64 `json:"vpn_rx_ip_bytes_during_request"`

	Host      string `json:"host"`
	Protocol  string `json:"protocol,omitempty"`
	Status    int    `json:"status"`
	Bytes     int64  `json:"bytes"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Complete  bool   `json:"complete"`
	Error     string `json:"error,omitempty"`
}

var diagnosticHosts = []string{"mail.google.com", "accounts.google.com", "www.gstatic.com", "ssl.gstatic.com"}

func diagnosticNetwork(n NetworkOptions, addresses map[string]netip.Addr) (NetworkOptions, error) {
	n.Routes = nil
	n.DNS = nil
	n.BlockIPv6 = false
	n.DNSService = ""
	n.Server = false
	seen := map[netip.Addr]bool{}
	for _, host := range diagnosticHosts {
		ip, ok := addresses[host]
		if !ok || !ip.Is4() || !publicDestination(ip) || ip == n.Endpoint || n.Network.Contains(ip) {
			return n, fmt.Errorf("invalid public diagnostic address for %s", host)
		}
		if !seen[ip] {
			n.Routes = append(n.Routes, netip.PrefixFrom(ip, 32))
			seen[ip] = true
		}
	}
	sort.Slice(n.Routes, func(i, j int) bool { return n.Routes[i].String() < n.Routes[j].String() })
	return n, nil
}

func Diagnose(parent context.Context, p *Prepared) (report Diagnostic) {
	report.Stage = "config"
	if p == nil || p.Role != "client" {
		report.Error = "enable client vpn-mode to diagnose"
		return
	}
	ctx, cancel := context.WithTimeout(parent, 180*time.Second)
	defer cancel()
	c, n, err := p.Resolve(ctx)
	if err != nil {
		report.Error = err.Error()
		return
	}
	report.Stage = "probe"
	report.Probe = Probe(ctx, c)
	if !report.Probe.OK {
		report.Error = report.Probe.Error
		return
	}
	if report.Probe.AddressInUse {
		report.Error = "VPN virtual IP is already in use; stop the sgate VPN client before diagnosing (OpenVPN may remain connected)"
		return
	}
	if report.Probe.ClientLimitReached {
		report.Error = "VPN server client limit reached; diagnostic did not open a TUN"
		return
	}
	report.Stage = "resolve"
	addresses := map[string]netip.Addr{}
	for _, host := range diagnosticHosts {
		resolveCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		ips, e := net.DefaultResolver.LookupNetIP(resolveCtx, "ip4", host)
		stop()
		if e != nil || len(ips) == 0 {
			report.Error = "resolve " + host + ": no usable IPv4 answer"
			return
		}
		addresses[host] = ips[0].Unmap()
	}
	n, err = diagnosticNetwork(n, addresses)
	if err != nil {
		report.Error = err.Error()
		return
	}
	for _, r := range n.Routes {
		report.Routes = append(report.Routes, r.String())
	}
	report.Stage = "tun"
	dev, err := OpenTUN("")
	if err != nil {
		report.Error = err.Error()
		return
	}
	report.Device = dev.Name()
	n.Interface = dev.Name()
	defer dev.Close()
	// Closing our own device interrupts any pending I/O on timeout/shutdown.
	interrupt := context.AfterFunc(ctx, func() { dev.Close() })
	defer interrupt()
	stats := &Stats{}
	c.Stats = stats
	c.Noise = NoiseConfig{}
	var setupMu sync.Mutex
	var setupError error
	assignedIP := c.ClientIP
	c.Assigned = func(ip netip.Addr) { setupMu.Lock(); assignedIP = ip; n.ClientIP = ip; setupMu.Unlock() }
	c.Connected = func() (func(), error) {
		undo, e := ApplyNetwork(n)
		setupMu.Lock()
		setupError = e
		setupMu.Unlock()
		return undo, e
	}
	runCtx, stopClient := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- RunClient(runCtx, c, dev) }()
	defer func() {
		stopClient()
		dev.Close()
		// RunClient owns network cleanup. Join it before returning the report.
		<-done
		report.Stats = stats.Snapshot()
	}()
	report.Stage = "connect"
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for stats.Snapshot().Active == 0 {
		select {
		case <-ctx.Done():
			report.Error = ctx.Err().Error()
			return
		case e := <-done:
			// Keep the deferred join balanced after consuming the only completion.
			done <- e
			setupMu.Lock()
			setup := setupError
			setupMu.Unlock()
			if setup != nil {
				report.Error = setup.Error()
			} else if e != nil {
				report.Error = e.Error()
			} else {
				report.Error = "VPN ended before connecting"
			}
			return
		case <-ticker.C:
		}
	}
	setupMu.Lock()
	c.ClientIP = assignedIP
	setupMu.Unlock()
	report.Stage = "https"
	dialer := &net.Dialer{Timeout: 8 * time.Second, LocalAddr: &net.TCPAddr{IP: c.ClientIP.AsSlice()}}
	if err := bindDiagnosticInterface(dialer, dev.Name()); err != nil {
		report.Error = err.Error()
		return
	}
	transport := &http.Transport{Proxy: nil, ForceAttemptHTTP2: true, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		ip, ok := addresses[host]
		if !ok {
			return nil, errors.New("redirect outside diagnostic host allowlist")
		}
		conn, err := dialer.DialContext(ctx, "tcp4", net.JoinHostPort(ip.String(), port))
		if err == nil {
			local, ok := conn.LocalAddr().(*net.TCPAddr)
			if !ok || !local.IP.Equal(c.ClientIP.AsSlice()) {
				conn.Close()
				return nil, errors.New("diagnostic TCP socket bypassed the test VPN interface")
			}
		}
		return conn, err
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 45 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || addresses[req.URL.Hostname()] == (netip.Addr{}) || req.URL.Scheme != "https" {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	for _, host := range diagnosticHosts {
		endpoint := url.URL{Scheme: "https", Host: host, Path: "/"}
		if host == "mail.google.com" {
			endpoint.Path = "/mail/u/0/"
		}
		began := time.Now()
		result := HTTPDiagnostic{Host: host}
		before := stats.Snapshot()
		var traceMu sync.Mutex
		trace := &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				traceMu.Lock()
				result.LocalAddress = info.Conn.LocalAddr().String()
				result.RemoteAddress = info.Conn.RemoteAddr().String()
				traceMu.Unlock()
			},
			GotFirstResponseByte: func() { traceMu.Lock(); result.FirstByteMS = time.Since(began).Milliseconds(); traceMu.Unlock() },
		}
		requestCtx := httptrace.WithClientTrace(ctx, trace)
		req, e := http.NewRequestWithContext(requestCtx, "GET", endpoint.String(), nil)
		if e == nil {
			var response *http.Response
			response, e = client.Do(req)
			if response != nil {
				result.Uncompressed = response.Uncompressed
				result.Status = response.StatusCode
				result.Protocol = response.Proto
				// Read at most 2 MiB. Reaching the sample cap is not complete delivery.
				result.Bytes, e = io.Copy(io.Discard, io.LimitReader(response.Body, (2<<20)+1))
				response.Body.Close()
				result.Complete = e == nil && result.Bytes <= 2<<20
				if result.Bytes > 2<<20 {
					e = errors.New("public response exceeds diagnostic sample limit")
				}
			}
		}
		if e != nil {
			result.Error = e.Error()
		}
		// All response trace callbacks have completed before finalizing this result.
		traceMu.Lock()
		result.ElapsedMS = time.Since(began).Milliseconds()
		after := stats.Snapshot()
		result.IPTXBytes = after.TX - before.TX
		result.IPRXBytes = after.RX - before.RX
		report.Results = append(report.Results, result)
		traceMu.Unlock()
	}
	report.Stage = "complete"
	for _, result := range report.Results {
		if !result.Complete {
			report.Error = "one or more public HTTPS responses were not completely received; see https results"
			break
		}
	}
	return
}
