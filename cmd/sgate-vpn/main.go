// sgate-vpn is an independent TUN / QUIC DATAGRAM endpoint.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"sgate/internal/cli"
	"sgate/internal/vpn"
)

type options struct {
	accountsFile, credentialsFile                                                                         string
	requireAccount                                                                                        bool
	tcpListen                                                                                             string
	udpListen                                                                                             string
	role, listen, server, keyFile, cert, key, ca, network, clientIP, tun, routes, dns, dnsService, egress string
	noise                                                                                                 vpn.NoiseConfig
	queueMaxAge, udpSendInterval                                                                          time.Duration
	mtu, maxClients                                                                                       int
	tcpNotSentLowat                                                                                       int
	manage, private, block6                                                                               bool
}

func parse(args []string) (options, error) {
	o := options{}
	fs := flag.NewFlagSet("sgate-vpn", flag.ContinueOnError)
	fs.BoolVar(&o.requireAccount, "require-account", false, "require VPN account authentication")
	fs.StringVar(&o.accountsFile, "accounts-file", "", "dedicated VPN server accounts JSON")
	fs.StringVar(&o.credentialsFile, "credentials-file", "", "dedicated VPN client credentials JSON")
	fs.StringVar(&o.role, "role", "client", "client or server")
	fs.StringVar(&o.listen, "listen", ":8444", "server QUIC UDP listen address")
	fs.StringVar(&o.udpListen, "udp-listen", "", "optional encrypted raw UDP listener; separate from QUIC UDP port")
	fs.StringVar(&o.tcpListen, "tcp-listen", "", "optional shared TLS/TCP and WSS server listener; empty disables")
	cli.ByteSizeVar(fs, &o.tcpNotSentLowat, "tcp-notsent-lowat", 0, "Linux TCP/WSS unsent-data low-water mark; 0 uses system default, maximum 1M")
	fs.StringVar(&o.server, "server", "", "client udp://, quic://, tcp:// or wss://hostname:port/vpn URL (TCP always uses TLS)")
	fs.StringVar(&o.keyFile, "key-file", "", "32-byte shared key file (same format as sgate)")
	fs.StringVar(&o.cert, "tls-cert", "", "server certificate PEM")
	fs.StringVar(&o.key, "tls-key", "", "server private key PEM")
	fs.StringVar(&o.ca, "tls-ca", "", "client additional trusted CA PEM")
	fs.StringVar(&o.network, "vpn-network", "10.77.0.0/24", "IPv4 VPN subnet, identical on both ends")
	fs.StringVar(&o.clientIP, "client-ip", "", "deprecated; ignored, address is assigned by server")
	fs.StringVar(&o.tun, "tun-name", "", "new TUN name (Linux: sgate0; macOS: auto utun)")
	fs.IntVar(&o.mtu, "mtu", 1280, "IPv4 TUN MTU, 576..1500")
	fs.IntVar(&o.maxClients, "max-clients", 64, "maximum authenticated clients (server)")
	fs.BoolVar(&o.manage, "manage-network", true, "configure owned TUN/routes/DNS or server NAT and restore on exit; requires administrator")
	fs.BoolVar(&o.private, "allow-private", false, "allow clients to reach private server-side destinations")
	fs.BoolVar(&o.block6, "block-ipv6", true, "client routes IPv6 into unsupported TUN and discards it instead of bypassing VPN")
	fs.StringVar(&o.routes, "routes", "0.0.0.0/1,128.0.0.0/1", "client IPv4 prefixes to route into VPN; 0.0.0.0/0 is split into two routes")
	fs.StringVar(&o.dns, "dns", "1.1.1.1", "client IPv4 DNS servers, comma separated; empty leaves DNS unchanged")
	fs.StringVar(&o.dnsService, "dns-service", "Wi-Fi", "macOS network service whose DNS is temporarily changed")
	cli.ByteSizeVar(fs, &o.noise.Rate, "noise-rate", 0, "VPN padding payload bytes per second per connection (K/M supported), 0 disables, maximum 10M")
	fs.BoolVar(&o.noise.Traffic, "noise-traffic", true, "allow VPN padding during IP traffic; business packets have priority")
	fs.BoolVar(&o.noise.Idle, "noise-idle", true, "allow VPN padding while no business IP data flows")
	fs.DurationVar(&o.queueMaxAge, "queue-max-age", 0, "queued IP packet expiry, 0 disables (recommended for weak-link TCP), maximum 1m")
	fs.DurationVar(&o.udpSendInterval, "udp-send-interval", 0, "minimum gap between bulk UDP VPN packets, 0 disables, maximum 10ms")
	fs.DurationVar(&o.noise.IdleAfter, "noise-idle-after", time.Second, "VPN IP inactivity threshold, 0 uses 1s")
	fs.StringVar(&o.egress, "egress-interface", "", "Linux server physical egress interface; empty detects current route")
	if err := cli.Parse(fs, args); err != nil {
		return o, err
	}
	if o.role != "client" && o.role != "server" {
		return o, errors.New("role must be client or server")
	}
	if configPath := fs.Lookup("config").Value.String(); configPath != "" {
		for _, path := range []*string{&o.accountsFile, &o.credentialsFile} {
			if *path != "" && !filepath.IsAbs(*path) {
				*path = filepath.Join(filepath.Dir(configPath), *path)
			}
		}
	}
	return o, nil
}
func main() {
	if err := run(os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Print(err)
		os.Exit(1)
	}
}
func run(args []string) error {
	o, err := parse(args)
	if err != nil {
		return err
	}
	network, err := netip.ParsePrefix(o.network)
	if err != nil {
		return err
	}
	var clientIP netip.Addr
	if o.role == "server" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return errors.New("VPN server currently requires macOS or Linux")
	}
	key, err := cli.LoadKey(o.keyFile)
	if err != nil {
		return err
	}
	stats := &vpn.Stats{}
	config := vpn.Config{RequireAccount: o.requireAccount, QueueMaxAge: o.queueMaxAge, UDPSendInterval: o.udpSendInterval, Noise: o.noise, Key: key, Network: network, ClientIP: clientIP, MTU: o.mtu, MaxClients: o.maxClients, AllowPrivate: o.private, Stats: stats, Logf: log.Printf}
	accountPath := o.credentialsFile
	if o.role == "server" {
		accountPath = o.accountsFile
	}
	if (o.role == "server" && o.credentialsFile != "") || (o.role == "client" && o.accountsFile != "") {
		return errors.New("use accounts-file for a VPN server and credentials-file for a VPN client")
	}
	if err := config.LoadAccountFile(accountPath, o.role == "server"); err != nil {
		return err
	}
	netOptions := vpn.NetworkOptions{Server: o.role == "server", Network: network, ClientIP: clientIP, MTU: o.mtu, DNSService: o.dnsService, Egress: o.egress, BlockIPv6: o.block6, Logf: log.Printf}
	if !network.Addr().Is4() || network != network.Masked() || network.Bits() < 16 || network.Bits() > 30 || o.mtu < 576 || o.mtu > 1500 {
		return errors.New("invalid IPv4 VPN subnet or MTU")
	}
	if o.role == "server" {
		config.Address = o.listen
		config.TCPListen = o.tcpListen
		config.TCPNotSentLowat = o.tcpNotSentLowat
		config.UDPListen = o.udpListen
		config.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
		if o.listen != "" || o.tcpListen != "" {
			cert, err := tls.LoadX509KeyPair(o.cert, o.key)
			if err != nil {
				return err
			}
			config.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
		}
		config.Address = o.listen
		config.TCPListen = o.tcpListen
	} else {
		u, err := url.Parse(o.server)
		if err != nil || (u.Scheme != "quic" && u.Scheme != "tcp" && u.Scheme != "wss" && u.Scheme != "udp") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/vpn") {
			return errors.New("server must be udp://, quic://, tcp:// or wss://hostname:port/vpn")
		}
		config.Transport, config.Host = u.Scheme, u.Host
		host, port, err := net.SplitHostPort(u.Host)
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid VPN server port")
		}
		resolveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		addresses, err := net.DefaultResolver.LookupNetIP(resolveCtx, "ip4", host)
		cancel()
		if err != nil {
			return err
		}
		if len(addresses) == 0 {
			return errors.New("VPN server needs an IPv4 endpoint")
		}
		endpoint := addresses[0].Unmap()
		netOptions.Endpoint = endpoint
		config.Address = net.JoinHostPort(endpoint.String(), port)
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if o.ca != "" && config.Transport != "udp" {
			data, err := os.ReadFile(o.ca)
			if err != nil {
				return err
			}
			if !roots.AppendCertsFromPEM(data) {
				return errors.New("tls-ca contains no certificates")
			}
		}
		config.TLS = &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS13}
		if o.routes != "" {
			for _, raw := range strings.Split(o.routes, ",") {
				prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
				if err != nil || !prefix.Addr().Is4() {
					return errors.New("routes must be IPv4 prefixes")
				}
				if prefix.Bits() == 0 {
					netOptions.Routes = append(netOptions.Routes, netip.MustParsePrefix("0.0.0.0/1"), netip.MustParsePrefix("128.0.0.0/1"))
				} else {
					netOptions.Routes = append(netOptions.Routes, prefix.Masked())
				}
			}
		}
		if o.dns != "" {
			for _, raw := range strings.Split(o.dns, ",") {
				ip, err := netip.ParseAddr(strings.TrimSpace(raw))
				if err != nil || !ip.Is4() || ip == endpoint {
					return errors.New("DNS must be IPv4 addresses different from the VPN endpoint")
				}
				netOptions.DNS = append(netOptions.DNS, ip)
			}
		}
	}
	// All flags, credentials and certificates are validated before opening a device.
	if err := vpn.ValidateConfig(config, o.role == "server"); err != nil {
		return err
	}
	device, err := vpn.OpenTUN(o.tun)
	if err != nil {
		return fmt.Errorf("open VPN device (administrator privileges required): %w", err)
	}
	defer device.Close()
	netOptions.Interface = device.Name()
	log.Printf("VPN role=%s device=%s network=%s mtu=%d", o.role, device.Name(), network, o.mtu)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		timer := time.NewTicker(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				b, _ := json.Marshal(stats.Snapshot())
				log.Printf("VPN stats %s", b)
			}
		}
	}()
	if o.role == "server" {
		if o.manage {
			undo, err := vpn.ApplyNetwork(netOptions)
			if err != nil {
				return err
			}
			defer undo()
		}
		server, err := vpn.ListenServer(config, device)
		if err != nil {
			return err
		}
		defer server.Close()
		log.Printf("VPN listening: %s", server.Addr())
		return server.Serve(ctx)
	}
	config.Assigned = func(ip netip.Addr) { netOptions.ClientIP = ip }
	if o.manage {
		config.Connected = func() (func(), error) { return vpn.ApplyNetwork(netOptions) }
	}
	return vpn.RunClient(ctx, config, device)
}
