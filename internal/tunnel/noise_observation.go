package tunnel

import "time"

// NoiseActivitySnapshot 区分在线传输和离线保留的业务会话；心跳及噪音不算业务。
type NoiseActivitySnapshot struct {
	ReceiveBufferBytes int    `json:"receive_buffer_bytes"`
	ReplayBufferBytes  int    `json:"replay_buffer_bytes"`
	Sessions           int    `json:"sessions"`
	Streams            int    `json:"streams"`
	Offline            int    `json:"offline"`
	OfflineStreams     int    `json:"offline_streams"`
	Disconnects        uint64 `json:"disconnects"`
	LastDisconnect     string `json:"last_disconnect,omitempty"`
	LastDisconnectTime int64  `json:"last_disconnect_ms,omitempty"`
	Tunnels            int    `json:"tunnels"`
	Traffic            int    `json:"traffic"`
	Idle               int    `json:"idle"`
	Empty              int    `json:"empty"`
	Permitted          int    `json:"permitted"`
	SuppressedTraffic  int    `json:"suppressed_traffic"`
	SuppressedIdle     int    `json:"suppressed_idle"`
	SuppressedEmpty    int    `json:"suppressed_empty"`
	Disabled           int    `json:"disabled"`
}

// registerNoiseTransport 只持有当前写协程对应的会话；所有退出路径必须注销。
func (s *Stats) registerNoiseTransport(current *session) func() {
	s.mu.Lock()
	if s.noiseSessions == nil {
		s.noiseSessions = make(map[*session]int)
	}
	s.noiseSessions[current]++
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if s.noiseSessions[current] <= 1 {
			delete(s.noiseSessions, current)
		} else {
			s.noiseSessions[current]--
		}
		s.mu.Unlock()
	}
}

// 会话注册覆盖断线保留窗口，close 时删除，不增加定时器或后台协程。
func (s *Stats) registerSession(current *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[*session]struct{})
	}
	s.sessions[current] = struct{}{}
}
func (s *Stats) unregisterSession(current *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, current)
}
func (s *Stats) recordDisconnect(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disconnects++
	s.lastDisconnectTime = time.Now().UnixMilli()
	s.lastDisconnect = "连接已关闭"
	if err != nil {
		// 仅保留最后一个错误，避免频繁断线造成诊断内存持续增长。
		s.lastDisconnect = err.Error()
		if len(s.lastDisconnect) > 1024 {
			s.lastDisconnect = s.lastDisconnect[:1024]
		}
	}
}

func (s *Stats) noiseActivity() NoiseActivitySnapshot {
	// 不嵌套统计锁和会话锁，避免与关闭或热更新产生锁顺序依赖。
	s.mu.Lock()
	sessions := make(map[*session]bool, len(s.sessions))
	for session := range s.sessions {
		sessions[session] = s.noiseSessions[session] > 0
	}
	result := NoiseActivitySnapshot{Disconnects: s.disconnects, LastDisconnect: s.lastDisconnect, LastDisconnectTime: s.lastDisconnectTime}
	s.mu.Unlock()
	now := time.Now()
	for session, online := range sessions {
		options, _ := session.opts.liveSnapshot()
		session.mu.Lock()
		incomingBytes, pendingBytes := session.incomingBytes, session.pendingBytes
		state, closed, streams := session.activityLocked(now, options.NoiseIdleAfter), session.closed, len(session.streams)
		session.mu.Unlock()
		if closed {
			continue
		}
		result.ReceiveBufferBytes += incomingBytes
		result.ReplayBufferBytes += pendingBytes
		result.Sessions++
		result.Streams += streams
		if !online {
			result.Offline++
			result.OfflineStreams += streams
			continue
		}
		result.Tunnels++
		allowed := false
		switch state {
		case noiseTraffic:
			result.Traffic++
			allowed = !options.NoiseSuppressTraffic
		case noiseIdle:
			result.Idle++
			allowed = !options.NoiseSuppressIdle
		case noiseEmpty:
			result.Empty++
			allowed = !options.NoiseSuppressEmpty
		}
		if options.NoiseInterval == 0 && options.NoiseIntervalMin == 0 {
			result.Disabled++
			continue
		}
		if allowed {
			result.Permitted++
			continue
		}
		switch state {
		case noiseTraffic:
			result.SuppressedTraffic++
		case noiseIdle:
			result.SuppressedIdle++
		case noiseEmpty:
			result.SuppressedEmpty++
		}
	}
	return result
}
