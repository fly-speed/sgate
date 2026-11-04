package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

func googleHost(host string) bool {
	for _, suffix := range []string{"google.com", "gstatic.com", "googleusercontent.com", "googleapis.com"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// CONNECT leaves TLS and browser cookies opaque. Reports contain only connection
// metadata. The PAC routes other browser traffic normally, keeping chat online.
func serveBrowserProxy(ctx context.Context, listen string, dial func(context.Context, string, string) (net.Conn, error), report map[string]any, save func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host != "127.0.0.1" {
		return fmt.Errorf("proxy must bind 127.0.0.1")
	}
	ln, err := net.Listen("tcp4", listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	address := ln.Addr().String()
	report["proxy_address"] = address
	report["pac_url"] = "http://" + address + "/proxy.pac"
	save()
	var mu sync.Mutex
	var entries []map[string]any
	var workers sync.WaitGroup
	var gate sync.Mutex
	closing := false
	slots := make(chan struct{}, 32)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/proxy.pac" {
			w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
			fmt.Fprintf(w, `function FindProxyForURL(url, host) { host=host.toLowerCase(); var domains=["google.com","gstatic.com","googleusercontent.com","googleapis.com"]; for(var i=0;i<domains.length;i++){ if(host===domains[i] || dnsDomainIs(host,"."+domains[i])) return "PROXY %s"; } return "DIRECT"; }`, address)
			return
		}
		host, port, e := net.SplitHostPort(r.Host)
		if r.Method != "CONNECT" || e != nil || port != "443" || !googleHost(host) {
			http.Error(w, "diagnostic Google HTTPS only", 403)
			return
		}
		gate.Lock()
		if closing {
			gate.Unlock()
			http.Error(w, "diagnostic stopping", 503)
			return
		}
		workers.Add(1)
		gate.Unlock()
		defer workers.Done()
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			return
		}

		began := time.Now()
		entry := map[string]any{"host": host, "started_utc": began.UTC().Format(time.RFC3339)}
		defer func() {
			entry["elapsed_ms"] = time.Since(began).Milliseconds()
			mu.Lock()
			if len(entries) < 512 {
				entries = append(entries, entry)
			}
			report["connections"] = entries
			save()
			mu.Unlock()
		}()
		dc, cancel := context.WithTimeout(ctx, 15*time.Second)
		upstream, e := dial(dc, "tcp4", net.JoinHostPort(host, port))
		cancel()
		entry["connect_ms"] = time.Since(began).Milliseconds()
		if e != nil {
			entry["error"] = e.Error()
			http.Error(w, "VPN destination connect failed", 502)
			return
		}
		defer upstream.Close()
		client, buffer, e := w.(http.Hijacker).Hijack()
		if e != nil {
			entry["error"] = e.Error()
			return
		}
		defer client.Close()
		watch := make(chan struct{})
		defer close(watch)
		go func() {
			select {
			case <-ctx.Done():
				client.Close()
				upstream.Close()
			case <-watch:
			}
		}()
		if deadline, ok := ctx.Deadline(); ok {
			client.SetDeadline(deadline)
			upstream.SetDeadline(deadline)
		}
		buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffer.Flush()
		entry["remote"] = upstream.RemoteAddr().String()
		type result struct {
			n   int64
			err error
		}
		sent := make(chan result, 1)
		go func() { n, e := io.Copy(upstream, buffer); upstream.Close(); sent <- result{n, e} }()
		received, e := io.Copy(client, upstream)
		client.Close()
		upstream.Close()
		tx := <-sent
		entry["tx_bytes"] = tx.n
		entry["rx_bytes"] = received
		if e != nil {
			entry["read_error"] = e.Error()
		}
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ln) }()
	fmt.Printf("PAC URL: http://%s/proxy.pac\n", address)
	select {
	case <-ctx.Done():
	case err = <-done:
	}
	gate.Lock()
	closing = true
	gate.Unlock()
	cancel()
	server.Close()
	workers.Wait()
	report["finished"] = true
	return err
}

// Resolve once per host so concurrent resource requests use the same endpoint.
// Resolution follows the selected diagnostic DNS source, never browser DNS.
type proxyLookup func(context.Context, string) ([]string, error)

func resolvedProxyDial(dial func(context.Context, string, string) (net.Conn, error), lookup proxyLookup) func(context.Context, string, string) (net.Conn, error) {
	var mu sync.Mutex
	ips := map[string]string{}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		ip := ips[host]
		mu.Unlock()
		if ip == "" {
			answers, err := lookup(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, answer := range answers {
				parsed := net.ParseIP(answer)
				if parsed != nil && parsed.To4() != nil && parsed.IsGlobalUnicast() && !parsed.IsPrivate() {
					ip = parsed.String()
					break
				}
			}
			if ip == "" {
				return nil, fmt.Errorf("no IPv4 address for %s", host)
			}
			mu.Lock()
			if prior := ips[host]; prior != "" {
				ip = prior
			} else {
				ips[host] = ip
			}
			mu.Unlock()
		}
		return dial(ctx, "tcp4", net.JoinHostPort(ip, port))
	}
}
