package tunnel

import "io"

// queueIncoming 保留未交付的数据；ACK 仅在业务 TCP 写入完成后前移。
// accepted 跨重连保存，使“已入队但尚未确认”的重放帧不会再次交付。
func (s *session) queueIncoming(f frame, stopped <-chan struct{}) error {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return errSessionClosed
		}
		if f.seq <= s.accepted {
			s.signalLocked()
			s.mu.Unlock()
			return nil
		}
		if f.seq != s.accepted+1 {
			s.mu.Unlock()
			return errReceiveGap
		}
		cost := frameHeader + len(f.data)
		if s.incomingBytes+cost <= s.opts.BufferBytes {
			s.incoming = append(s.incoming, f)
			s.incomingBytes += cost
			s.accepted = f.seq
			s.signalLocked()
			s.mu.Unlock()
			return nil
		}
		changed := s.changed
		s.mu.Unlock()
		// 老版本未协商窗口时仍有界等待；新版发送端按本端公布的窗口限制在途数据。
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-stopped:
			return io.ErrClosedPipe
		case <-changed:
		}
	}
}

// 业务 TCP 的背压不能堵住 WebSocket 读者，否则噪音、ACK、心跳都会停止消费。
// 整个会话只有一个有界交付队列，保留原有的跨流全局顺序。
func (s *session) deliverIncoming() {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		if len(s.incoming) == 0 {
			changed := s.changed
			s.mu.Unlock()
			select {
			case <-s.ctx.Done():
				return
			case <-changed:
				continue
			}
		}
		f := s.incoming[0]
		s.mu.Unlock()
		if err := s.apply(f); err != nil {
			s.opts.Logf("business delivery failed: %v", err)
			s.close()
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		s.incoming[0] = frame{}
		s.incoming = s.incoming[1:]
		if len(s.incoming) == 0 {
			s.incoming = nil
		}
		s.incomingBytes -= frameHeader + len(f.data)
		s.received = f.seq
		s.signalLocked()
		s.mu.Unlock()
	}
}
