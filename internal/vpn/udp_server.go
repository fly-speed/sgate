package vpn

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"github.com/pion/dtls/v3"
)

func (s *Server) listenUDP() error {
	a, err := net.ResolveUDPAddr("udp", s.cfg.UDPListen)
	if err != nil {
		return err
	}
	s.udp, err = dtls.Listen("udp", a, udpSecurity(s.cfg.Key))
	return err
}
func (s *Server) acceptUDP(ctx context.Context) {
	if s.udp == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		var admitted atomic.Int64
		for {
			raw, err := s.udp.Accept()
			if err != nil {
				if ctx.Err() == nil {
					select {
					case s.acceptErr <- err:
					default:
					}
				}
				return
			}
			if admitted.Add(1) > int64(max(s.cfg.Live.Snapshot().MaxClients, int(s.cfg.Stats.active.Load()))+16) {
				admitted.Add(-1)
				raw.Close()
				continue
			}
			s.udpConns.Store(raw, struct{}{})
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer admitted.Add(-1)
				defer s.udpConns.Delete(raw)
				defer raw.Close()
				authCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
				err := raw.(*dtls.Conn).HandshakeContext(authCtx)
				cancel()
				if err != nil {
					s.cfg.Stats.auth.Add(1)
					return
				}
				conn := newUDPConnection(raw, s.cfg.Stats)
				defer conn.CloseWithError(0, "VPN UDP listener stopped")
				select {
				case s.incoming <- conn:
				case <-ctx.Done():
					return
				}
				select {
				case <-conn.Context().Done():
				case <-ctx.Done():
				}
			}()
		}
	}()
}
