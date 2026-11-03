package vpn

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

type networkSetupError struct{ err error }

func (e *networkSetupError) Error() string { return "configure VPN network: " + e.err.Error() }

type peer struct {
	conn         connection
	control      controlChannel
	id           string
	ip           netip.Addr
	queue        *packetQueue
	padding      bool
	flow         *datagramFlow
	started      time.Time
	lastBusiness atomic.Int64
}

func (p *peer) enqueue(packet []byte, stats *Stats) {
	p.queue.push(packet, time.Now(), stats)
}
func (p *peer) run(ctx context.Context, c Config, receive func([]byte) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.started = time.Now()
	if c.Noise.Rate > 0 && !p.padding {
		c.Stats.noiseUnsupported.Add(1)
		defer c.Stats.noiseUnsupported.Add(-1)
	}
	done := make(chan error, 3)
	go func() {
		var id uint64
		var tick <-chan time.Time
		if p.padding && c.Noise.Rate > 0 {
			ticker := time.NewTicker(noiseTick)
			defer ticker.Stop()
			tick = ticker.C
		}
		pacer := noisePacer{last: time.Now()}
		send := func(packet []byte) error {
			if _, reliable := p.conn.(*recordConnection); !reliable && clampTCPMSS(packet, c.MTU) {
				c.Stats.mssClamped.Add(1)
			}
			id++
			var err error
			if reliable, ok := p.conn.(*recordConnection); ok {
				err = reliable.sendIP(packet)
			} else {
				err = fragments(packet, id, p.sendDatagram)
			}
			if err != nil {
				var tooLarge *quic.DatagramTooLargeError
				if errors.As(err, &tooLarge) {
					c.Stats.drops.Add(1)
					return nil
				}
				return err
			}
			c.Stats.tx.Add(uint64(len(packet)))
			p.businessActivity()
			return nil
		}

		for {
			if ctx.Err() != nil {
				done <- ctx.Err()
				return
			}
			// Leave packets in the scheduler while QUIC is congested, so a
			// bulk flow cannot fill the library FIFO ahead of new short flows.
			if p.queue.len() > 0 {
				if err := p.waitForAdmission(ctx, c.Stats); err != nil {
					done <- err
					return
				}
			}
			if packet := p.queue.pop(time.Now(), c.Stats); packet != nil {
				began := time.Now()
				err := send(packet)
				storeMax(&c.Stats.sendWaitMax, uint64(time.Since(began)))
				if err != nil {
					done <- err
					return
				}
				continue
			}
			select {
			case <-ctx.Done():
				done <- ctx.Err()
				return
			case <-p.queue.ready:
			case <-tick:
				if err := p.sendNoise(time.Now(), c, &pacer); err != nil {
					done <- err
					return
				}
			}
		}
	}()
	go func() {
		r := reassembler{stats: c.Stats}
		for {
			data, err := p.conn.ReceiveDatagram(ctx)
			if err != nil {
				done <- err
				return
			}
			if len(data) >= 2 && data[0] == 1 && data[1] == 1 {
				if p.padding && len(data) > 2 && len(data) <= fragmentPayload+2 {
					c.Stats.noiseRX.Add(uint64(len(data) - 2))
				} else {
					c.Stats.drops.Add(1)
				}
				continue
			}
			var packet []byte
			if _, reliable := p.conn.(*recordConnection); reliable && len(data) >= 2 && data[0] == 2 && data[1] == 0 {
				packet = data[2:]
			} else {
				packet, err = r.push(data, c.MTU, time.Now())
			}
			if err != nil {
				c.Stats.drops.Add(1)
				continue
			}
			if packet == nil {
				continue
			}
			if _, reliable := p.conn.(*recordConnection); !reliable && clampTCPMSS(packet, c.MTU) {
				c.Stats.mssClamped.Add(1)
			}
			p.businessActivity()
			began := time.Now()
			err = receive(packet)
			storeMax(&c.Stats.tunWriteMax, uint64(time.Since(began)))
			if err != nil {
				done <- err
				return
			}
		}
	}()
	go func() {
		var b [1]byte
		_, err := p.control.Read(b[:])
		if err == nil {
			err = errors.New("unexpected VPN control data")
		}
		done <- err
	}()
	var result error
	remaining := 3
	select {
	case result = <-done:
		remaining--
	case <-ctx.Done():
		result = ctx.Err()
	}
	cancel()
	p.conn.CloseWithError(0, "VPN connection ended")
	// All workers are bounded and close with the QUIC connection.
	for i := 0; i < remaining; i++ {
		<-done
	}
	return result
}

type Server struct {
	cfg            Config
	dev            Device
	listener       *quic.Listener
	tcp            net.Listener
	http           *http.Server
	incoming       chan connection
	acceptErr      chan error
	closed         chan struct{}
	reliableConns  sync.Map
	handlerMu      sync.Mutex
	handlerWG      sync.WaitGroup
	handlersClosed bool
	transport      *quic.Transport
	packet         net.PacketConn
	mu             sync.Mutex
	peers          map[netip.Addr]*peer
	writeMu        sync.Mutex
	once           sync.Once
	wg             sync.WaitGroup
}

func ListenServer(config Config, dev Device) (*Server, error) {
	c, err := config.normalized(true)
	if err != nil {
		return nil, err
	}
	if dev == nil {
		return nil, errors.New("VPN packet device required")
	}
	if len(c.TLS.Certificates) == 0 && c.TLS.GetCertificate == nil {
		return nil, errors.New("VPN server certificate required")
	}
	s := &Server{cfg: c, dev: dev, peers: make(map[netip.Addr]*peer), incoming: make(chan connection), acceptErr: make(chan error, 2), closed: make(chan struct{})}
	if c.TCPListen != "" {
		if err := s.listenReliable(); err != nil {
			return nil, err
		}
	}
	if c.Address == "" {
		return s, nil
	}
	udp, err := net.ListenPacket("udp", c.Address)
	if err != nil {
		if s.tcp != nil {
			s.tcp.Close()
		}
		return nil, err
	}
	var admitted atomic.Int64
	tr := &quic.Transport{Conn: udp, ConnContext: func(ctx context.Context, _ *quic.ClientInfo) (context.Context, error) {
		if admitted.Add(1) > int64(c.MaxClients+16) {
			admitted.Add(-1)
			return nil, errors.New("VPN connection limit")
		}
		context.AfterFunc(ctx, func() { admitted.Add(-1) })
		return context.WithValue(ctx, flowContextKey{}, newDatagramFlow()), nil
	}}
	listener, err := tr.Listen(c.TLS, quicConfig(c.Stats))
	if err != nil {
		tr.Close()
		udp.Close()
		if s.tcp != nil {
			s.tcp.Close()
		}
		return nil, err
	}
	s.listener, s.transport, s.packet = listener, tr, udp
	return s, nil
}
func (s *Server) Addr() net.Addr {
	if s.listener != nil {
		return s.listener.Addr()
	}
	return s.tcp.Addr()
}
func (s *Server) TCPAddr() net.Addr {
	if s.tcp != nil {
		return s.tcp.Addr()
	}
	return nil
}
func (s *Server) Close() error {
	s.once.Do(func() {
		s.handlerMu.Lock()
		s.handlersClosed = true
		s.handlerMu.Unlock()
		close(s.closed)
		if s.listener != nil {
			s.listener.Close()
		}
		if s.http != nil {
			s.http.Close()
		}
		if s.tcp != nil {
			s.tcp.Close()
		}
		s.reliableConns.Range(func(k, v any) bool { k.(*recordConnection).CloseWithError(0, "VPN stopped"); return true })
		s.mu.Lock()
		for _, p := range s.peers {
			p.conn.CloseWithError(0, "VPN stopped")
		}
		s.mu.Unlock()
		if s.transport != nil {
			s.transport.Close()
		}
		if s.packet != nil {
			s.packet.Close()
		}
		s.dev.Close()
	})
	return nil
}
func (s *Server) Serve(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	deviceErr := make(chan error, 1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		buffer := make([]byte, 65535)
		for {
			n, err := s.dev.Read(buffer)
			if err != nil {
				if ctx.Err() == nil {
					deviceErr <- fmt.Errorf("VPN device read: %w", err)
					cancel()
				}
				return
			}
			packet := buffer[:n]
			_, dst, valid := ipv4(packet, s.cfg.MTU)
			if !valid {
				s.cfg.Stats.drops.Add(1)
				continue
			}
			s.mu.Lock()
			p := s.peers[dst]
			s.mu.Unlock()
			if p != nil {
				p.enqueue(packet, s.cfg.Stats)
			} else {
				s.cfg.Stats.drops.Add(1)
			}
		}
	}()
	var result error
	s.acceptConnections(ctx)
acceptLoop:
	for {
		select {
		case conn := <-s.incoming:
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.accept(ctx, conn) }()
		case err := <-s.acceptErr:
			if ctx.Err() == nil {
				result = err
			}
			break acceptLoop
		case <-ctx.Done():
			break acceptLoop
		}
	}
	cancel()
	s.Close()
	s.wg.Wait()
	s.handlerWG.Wait()
	select {
	case err := <-deviceErr:
		return err
	default:
		return result
	}
}
func (s *Server) accept(ctx context.Context, conn connection) {
	defer conn.CloseWithError(0, "VPN session ended")
	authCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	control, err := conn.AcceptStream(authCtx)
	if err != nil {
		return
	}
	control.SetDeadline(time.Now().Add(10 * time.Second))
	var preface [1]byte
	if _, err = io.ReadFull(control, preface[:]); err != nil || preface[0] != 1 {
		return
	}
	challenge, err := randomNonce()
	if err != nil {
		return
	}
	challenge = paddingChallengePrefix + challenge
	if writeJSON(control, challenge) != nil {
		return
	}
	var h hello
	if readJSON(control, &h) != nil {
		s.cfg.Stats.auth.Add(1)
		if s.cfg.Logf != nil {
			s.cfg.Logf("VPN authentication rejected: peer=%s reason=invalid-hello", conn.RemoteAddr())
		}
		conn.CloseWithError(authInvalidHelloCode, "invalid VPN authentication message")
		return
	}
	if !hmac.Equal([]byte(h.Proof), []byte(proof(s.cfg.Key, challenge, h))) {
		s.cfg.Stats.auth.Add(1)
		if s.cfg.Logf != nil {
			s.cfg.Logf("VPN authentication rejected: peer=%s probe=%t reason=shared-key-mismatch", conn.RemoteAddr(), h.Probe)
		}
		conn.CloseWithError(authKeyMismatchCode, "VPN shared key mismatch")
		return
	}
	ip, err := netip.ParseAddr(h.IP)
	reason := ""
	switch {
	case h.Network != s.cfg.Network.String():
		reason = "VPN network mismatch; expected vpn-network=" + s.cfg.Network.String()
	case h.MTU != s.cfg.MTU:
		reason = fmt.Sprintf("VPN MTU mismatch; expected vpn-mtu=%d", s.cfg.MTU)
	case err != nil || !validClientIP(s.cfg.Network, ip):
		reason = "invalid vpn-client-ip; choose an unused client host in " + s.cfg.Network.String()
	case len(h.ID) != 64:
		reason = "invalid VPN client identity"
	case !conn.supportsPackets():
		reason = "QUIC DATAGRAM was not negotiated"
	}
	if reason != "" {
		s.cfg.Stats.auth.Add(1)
		if s.cfg.Logf != nil {
			s.cfg.Logf("VPN authentication rejected: peer=%s probe=%t reason=%s", conn.RemoteAddr(), h.Probe, reason)
		}
		reject(control, reason)
		return
	}
	if h.Probe {
		s.serveProbe(authCtx, conn, control, challenge, h, ip)
		return
	}
	flow, _ := conn.Context().Value(flowContextKey{}).(*datagramFlow)
	p := &peer{conn: conn, control: control, id: h.ID, ip: ip, queue: newPacketQueue(s.cfg.QueueMaxAge), padding: h.Padding, flow: flow}
	s.mu.Lock()
	old := s.peers[ip]
	if (old != nil && old.id != h.ID) || (old == nil && len(s.peers) >= s.cfg.MaxClients) {
		s.mu.Unlock()
		reject(control, "VPN address in use or client limit reached")
		return
	}
	s.peers[ip] = p
	s.mu.Unlock()
	if old != nil {
		old.conn.CloseWithError(0, "VPN client reconnected")
	}
	defer func() {
		s.mu.Lock()
		if s.peers[ip] == p {
			delete(s.peers, ip)
		}
		s.mu.Unlock()
	}()
	reply := welcome{Padding: h.Padding, IP: ip.String(), Gateway: s.cfg.Network.Addr().Next().String(), Network: s.cfg.Network.String(), MTU: s.cfg.MTU}
	reply.Proof = welcomeProof(s.cfg.Key, challenge, h, reply)
	if writeJSON(control, reply) != nil {
		return
	}
	control.SetDeadline(time.Time{})
	s.cfg.Stats.connections.Add(1)
	s.cfg.Stats.active.Add(1)
	defer s.cfg.Stats.active.Add(-1)
	if s.cfg.Logf != nil {
		s.cfg.Logf("VPN client connected: address=%s peer=%s", ip, conn.RemoteAddr())
	}
	_ = p.run(ctx, s.cfg, func(packet []byte) error {
		src, dst, ok := ipv4(packet, s.cfg.MTU)
		gateway := s.cfg.Network.Addr().Next()
		if !ok || src != ip || (s.cfg.Network.Contains(dst) && dst != gateway) || (dst != gateway && !publicDestination(dst) && !s.cfg.AllowPrivate) {
			s.cfg.Stats.drops.Add(1)
			return nil
		}
		if !dst.IsGlobalUnicast() || dst.IsLoopback() || dst.IsLinkLocalUnicast() {
			s.cfg.Stats.drops.Add(1)
			return nil
		}
		s.writeMu.Lock()
		n, err := s.dev.Write(packet)
		s.writeMu.Unlock()
		if err == nil && n != len(packet) {
			err = io.ErrShortWrite
		}
		if err == nil {
			s.cfg.Stats.rx.Add(uint64(n))
		}
		return err
	})
}

func RunClient(parent context.Context, config Config, dev Device) error {
	c, err := config.normalized(false)
	if err != nil {
		return err
	}
	if dev == nil {
		return errors.New("VPN packet device required")
	}
	id, err := randomNonce()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var active atomic.Pointer[peer]
	deviceErr := make(chan error, 1)
	readerDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { dev.Close() })
	defer stop()
	go func() {
		defer close(readerDone)
		buffer := make([]byte, 65535)
		for {
			n, err := dev.Read(buffer)
			if err != nil {
				if ctx.Err() == nil {
					deviceErr <- err
					cancel()
				}
				return
			}
			src, _, ok := ipv4(buffer[:n], c.MTU)
			p := active.Load()
			if !ok || src != c.ClientIP || p == nil {
				c.Stats.drops.Add(1)
				continue
			}
			p.enqueue(buffer[:n], c.Stats)
		}
	}()
	var cleanup func()
	defer func() {
		if cleanup != nil {
			cleanup()
		}
		dev.Close()
		<-readerDone
	}()
	retry := c.RetryMin
	for ctx.Err() == nil {
		err = clientConnection(ctx, c, id, &active, dev, &cleanup)
		var setup *networkSetupError
		if errors.As(err, &setup) {
			return err
		}
		if ctx.Err() != nil {
			break
		}
		if c.Logf != nil {
			c.Logf("VPN connection unavailable: %v; retry in %s", err, retry)
		}
		timer := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		retry = min(retry*2, c.RetryMax)
	}
	select {
	case err := <-deviceErr:
		return fmt.Errorf("VPN device read: %w", err)
	default:
		return nil
	}
}
func clientConnection(ctx context.Context, c Config, id string, active *atomic.Pointer[peer], dev Device, cleanup *func()) error {
	var flow *datagramFlow
	if c.Transport == "" || c.Transport == "quic" {
		flow = newDatagramFlow()
	}
	conn, err := dialConnection(context.WithValue(ctx, flowContextKey{}, flow), c)
	if err != nil {
		return err
	}
	defer conn.CloseWithError(0, "VPN reconnecting")
	if !conn.supportsPackets() {
		return errors.New("server did not negotiate QUIC DATAGRAM")
	}
	authCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	control, err := conn.OpenStreamSync(authCtx)
	if err != nil {
		return err
	}
	control.SetDeadline(time.Now().Add(10 * time.Second))
	// Opening a QUIC stream is lazy. A preface makes it visible to AcceptStream.
	if _, err = control.Write([]byte{1}); err != nil {
		return err
	}
	var challenge string
	if err = readJSON(control, &challenge); err != nil {
		return err
	}
	h := hello{Padding: strings.HasPrefix(challenge, paddingChallengePrefix), ID: id, IP: c.ClientIP.String(), Network: c.Network.String(), MTU: c.MTU}
	h.Proof = proof(c.Key, challenge, h)
	if err = writeJSON(control, h); err != nil {
		return err
	}
	var reply welcome
	if err = readJSON(control, &reply); err != nil {
		return err
	}
	if reply.Error != "" {
		return errors.New(reply.Error)
	}
	if !hmac.Equal([]byte(reply.Proof), []byte(welcomeProof(c.Key, challenge, h, reply))) {
		return errors.New("invalid VPN server authentication proof")
	}
	if reply.IP != c.ClientIP.String() || reply.Network != c.Network.String() || reply.Gateway != c.Network.Addr().Next().String() || reply.MTU != c.MTU {
		return errors.New("invalid VPN server assignment")
	}
	control.SetDeadline(time.Time{})
	p := &peer{conn: conn, control: control, id: id, ip: c.ClientIP, queue: newPacketQueue(c.QueueMaxAge), padding: h.Padding && reply.Padding, flow: flow}
	if c.Connected != nil && *cleanup == nil {
		undo, err := c.Connected()
		if err != nil {
			if undo != nil {
				undo()
			}
			return &networkSetupError{err: err}
		}
		if undo == nil {
			undo = func() {}
		}
		*cleanup = undo
	}
	active.Store(p)
	defer active.CompareAndSwap(p, nil)
	c.Stats.connections.Add(1)
	c.Stats.active.Add(1)
	defer c.Stats.active.Add(-1)
	if c.Logf != nil {
		c.Logf("VPN connected: address=%s gateway=%s mtu=%d", c.ClientIP, reply.Gateway, c.MTU)
	}
	return p.run(ctx, c, func(packet []byte) error {
		_, dst, ok := ipv4(packet, c.MTU)
		if !ok || dst != c.ClientIP {
			c.Stats.drops.Add(1)
			return nil
		}
		n, err := dev.Write(packet)
		if err == nil && n != len(packet) {
			err = io.ErrShortWrite
		}
		if err == nil {
			c.Stats.rx.Add(uint64(n))
		}
		return err
	})
}

// FIN delivers the rejection before connection teardown discards queued stream
// bytes. A valid client closes promptly after reading it; stalled peers are bounded.
func reject(control controlChannel, reason string) {
	if writeJSON(control, welcome{Error: reason}) != nil {
		return
	}
	control.Close()
	control.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	control.Read(b[:])
}
