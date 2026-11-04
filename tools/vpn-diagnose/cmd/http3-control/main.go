package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

func main() {
	reference := flag.String("reference", "", "rootless HTTP3 report to pin identical IPs")
	output := flag.String("output", "", "new private output file")
	flag.Parse()
	data, err := os.ReadFile(*reference)
	if err != nil {
		panic(err)
	}
	var baseline struct {
		IPs map[string]string `json:"target_ips"`
	}
	if err = json.Unmarshal(data, &baseline); err != nil {
		panic(err)
	}
	file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		panic(err)
	}
	defer file.Close()
	tr := &http3.Transport{QUICConfig: &quic.Config{HandshakeIdleTimeout: 10 * time.Second, MaxIdleTimeout: 20 * time.Second, InitialPacketSize: 1200, DisablePathMTUDiscovery: true}, Dial: func(ctx context.Context, address string, tlsConfig *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		ip := baseline.IPs[host]
		if ip == "" {
			return nil, errors.New("destination outside pinned allowlist")
		}
		return quic.DialAddr(ctx, net.JoinHostPort(ip, port), tlsConfig, cfg)
	}}
	defer tr.Close()
	c := &http.Client{Transport: tr, Timeout: 25 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != "https" || baseline.IPs[req.URL.Hostname()] == "" {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	report := map[string]any{"scope": "host-route HTTP3 control using same pinned destinations; no fallback, cookies or route changes"}
	var results []map[string]any
	for _, host := range []string{"mail.google.com", "accounts.google.com"} {
		target := "https://" + host + "/"
		if host == "mail.google.com" {
			target += "mail/u/0/"
		}
		start := time.Now()
		r := map[string]any{"host": host, "ip": baseline.IPs[host]}
		response, e := c.Get(target)
		if response != nil {
			r["status"] = response.StatusCode
			r["protocol"] = response.Proto
			n, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, (4<<20)+1))
			response.Body.Close()
			r["body_bytes"] = n
			r["complete"] = readErr == nil && n <= 4<<20
			e = readErr
		}
		if e != nil {
			r["error"] = e.Error()
		}
		r["elapsed_ms"] = time.Since(start).Milliseconds()
		results = append(results, r)
		report["https"] = results
		file.Seek(0, 0)
		json.NewEncoder(file).Encode(report)
		pos, _ := file.Seek(0, 1)
		file.Truncate(pos)
		file.Sync()
		fmt.Fprintf(os.Stderr, "%s HTTP=%v complete=%v elapsed=%v error=%v\n", host, r["status"], r["complete"], r["elapsed_ms"], r["error"])
	}
}
