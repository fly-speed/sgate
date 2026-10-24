package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const stressApplicationDialTimeout = 5 * time.Second

type connectionCounter struct {
	active, accepted atomic.Int64
	socketBuffer     int // Set before accepting; bounds the load generator kernel queues.
}
type countedListener struct {
	net.Listener
	count *connectionCounter
}
type countedConn struct {
	net.Conn
	count *connectionCounter
	once  sync.Once
}

func (l countedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if l.count.socketBuffer > 0 {
		if err := stressSocketBuffer(c, l.count.socketBuffer); err != nil {
			c.Close()
			return nil, err
		}
	}
	l.count.active.Add(1)
	l.count.accepted.Add(1)
	return &countedConn{Conn: c, count: l.count}, nil
}
func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.count.active.Add(-1) })
	return err
}
func (c *countedConn) CloseWrite() error {
	return c.Conn.(interface{ CloseWrite() error }).CloseWrite()
}

type stressSample struct {
	SampleTimeUnixNano  int64  `json:"sample_time_unix_nano"`
	Phase               string `json:"phase"`
	Round               int    `json:"round"`
	Milliseconds        int64  `json:"milliseconds"`
	HeapBytes           uint64 `json:"heap_bytes"`
	HeapInuseBytes      uint64 `json:"heap_inuse_bytes"`
	HeapIdleBytes       uint64 `json:"heap_idle_bytes"`
	HeapReleasedBytes   uint64 `json:"heap_released_bytes"`
	StackBytes          uint64 `json:"stack_bytes"`
	SysBytes            uint64 `json:"sys_bytes"`
	RSSBytes            int64  `json:"rss_bytes,omitempty"`
	Goroutines          int    `json:"goroutines"`
	FileDescriptors     int    `json:"file_descriptors"`
	BackendConnections  int64  `json:"backend_connections"`
	LocalConnections    int64  `json:"local_connections"`
	HTTPConnections     int64  `json:"http_connections"`
	Sessions            int    `json:"sessions"`
	Streams             int    `json:"streams"`
	PendingBytes        int    `json:"pending_bytes"`
	Nonces              int    `json:"nonces"`
	NetworkMemoryDenied int64  `json:"network_memory_denied"`
	ClientSessions      int    `json:"client_sessions"`
	ClientStreams       int    `json:"client_streams"`
	ClientPendingBytes  int    `json:"client_pending_bytes"`
	ServerNoiseSent     uint64 `json:"server_noise_sent"`
	ServerNoiseReceived uint64 `json:"server_noise_received"`
	ClientNoiseSent     uint64 `json:"client_noise_sent"`
	ClientNoiseReceived uint64 `json:"client_noise_received"`
	RelayConnections    int64  `json:"relay_connections"`
	RelayQueuedBytes    int64  `json:"relay_queued_bytes"`
	RelayAccepted       int64  `json:"relay_accepted"`
	Retransmissions     int64  `json:"simulated_retransmissions"`
	TransportWriters    int    `json:"transport_writers"`
	RelayWrites         int64  `json:"relay_writes"`
	RelayCoalesced      int64  `json:"relay_coalesced_packets"`
	RelayWaitTimers     int64  `json:"relay_wait_timers"`
}
type stressReport struct {
	BackendHost                               string             `json:"backend_host"`
	ServerHost                                string             `json:"server_host"`
	BackendSocketsBound                       int                `json:"backend_sockets_bound"`
	CohortSettle                              string             `json:"cohort_settle"`
	LoadWindows                               []stressLoadWindow `json:"load_windows"`
	LoadSynchronized                          bool               `json:"load_synchronized"`
	Clients                                   int                `json:"clients"`
	StreamsPerClient                          int                `json:"streams_per_client"`
	PayloadBytes                              int                `json:"payload_bytes"`
	CompletedRounds                           int                `json:"completed_rounds"`
	Disconnects                               int64              `json:"disconnects"`
	VerifiedBytesPerDirection                 int64              `json:"verified_bytes_per_direction"`
	Samples                                   []stressSample     `json:"samples"`
	IndependentDuplex                         bool               `json:"independent_duplex"`
	VerifiedUploadBytes                       int64              `json:"verified_upload_bytes"`
	VerifiedDownloadBytes                     int64              `json:"verified_download_bytes"`
	NoiseIntervalMin                          string             `json:"noise_interval_min"`
	NoiseIntervalMax                          string             `json:"noise_interval_max"`
	NoiseMinBytes                             int                `json:"noise_min_bytes"`
	NoiseMaxBytes                             int                `json:"noise_max_bytes"`
	SocketBufferBytes                         int                `json:"socket_buffer_bytes"`
	RecordInterval                            string             `json:"record_interval"`
	WeakNetwork                               bool               `json:"weak_network"`
	WeakRelayBatchBytes                       int                `json:"weak_relay_batch_bytes"`
	WeakRelayDialTimeout                      string             `json:"weak_relay_dial_timeout"`
	ApplicationDialTimeout                    string             `json:"application_dial_timeout"`
	WeakDelay                                 string             `json:"weak_delay"`
	WeakJitter                                string             `json:"weak_jitter"`
	WeakRetransmit                            string             `json:"weak_retransmit"`
	WeakLossPercent                           int                `json:"weak_loss_percent"`
	ReconnectMin                              string             `json:"reconnect_min"`
	ReconnectMax                              string             `json:"reconnect_max"`
	ChurnInterval                             string             `json:"churn_interval"`
	OfflineDuration                           string             `json:"offline_duration"`
	TLS                                       bool               `json:"tls"`
	ServerRestarts                            int                `json:"server_restarts"`
	SessionExpiries                           int                `json:"session_expiry_phases"`
	InterruptedStreams                        int                `json:"intentionally_interrupted_streams"`
	SessionRetention                          string             `json:"session_retention"`
	RetentionOutage                           string             `json:"retention_outage"`
	RetainedSessionContinuity                 bool               `json:"retained_session_continuity"`
	BoundaryOutage                            string             `json:"boundary_outage"`
	BoundaryResumedSessions                   int                `json:"boundary_resumed_sessions"`
	BoundaryExpiredSessions                   int                `json:"boundary_expired_sessions"`
	BoundaryCompleteStreams                   int                `json:"boundary_complete_old_streams"`
	BoundaryInterruptedStreams                int                `json:"boundary_interrupted_old_streams"`
	BoundaryRestoreMilliseconds               []int64            `json:"boundary_restore_milliseconds"`
	BoundaryResolveMilliseconds               []int64            `json:"boundary_resolve_milliseconds"`
	UnconfirmedProbeWriteTimeout              string             `json:"unconfirmed_probe_write_timeout"`
	UnconfirmedUpgradeRetries                 int64              `json:"unconfirmed_upgrade_retries"`
	UnconfirmedMode                           string             `json:"unconfirmed_mode"`
	TLSHandshakeTimeouts                      int64              `json:"tls_handshake_timeouts"`
	AbandonedUpgrades                         int64              `json:"abandoned_upgrades"`
	SilentUpgrades                            int64              `json:"silent_upgrades"`
	UnconfirmedControlSent                    int64              `json:"unconfirmed_control_sent"`
	UnconfirmedPongsReceived                  int64              `json:"unconfirmed_pongs_received"`
	UnconfirmedFragmentsSent                  int64              `json:"unconfirmed_fragments_sent"`
	UnconfirmedPartialBytes                   int64              `json:"unconfirmed_partial_bytes"`
	UnconfirmedNoiseReceived                  int64              `json:"unconfirmed_noise_received"`
	UnconfirmedApplicationReleaseMilliseconds int64              `json:"unconfirmed_application_release_milliseconds"`
	UnconfirmedReleaseMilliseconds            int64              `json:"unconfirmed_release_milliseconds"`
}

type stressLoadWindow struct {
	Round             int   `json:"round"`
	Streams           int   `json:"streams"`
	ReadyUnixNano     int64 `json:"ready_unix_nano"`
	SendUntilUnixNano int64 `json:"send_until_unix_nano"`
}

func stressSocketBuffer(c net.Conn, size int) error {
	if err := c.(*net.TCPConn).SetReadBuffer(size); err != nil {
		return err
	}
	return c.(*net.TCPConn).SetWriteBuffer(size)
}

// Accepted backend sockets and outgoing proxy sockets are separate endpoints.
// Bound both; limiting only the accepted peer leaves the dialed receive buffer
// at the OS default, amplifying the all-local unread-cohort burst.
func stressBoundBackendSockets(t *testing.T, server *Server, size, expected int) int {
	t.Helper()
	if size == 0 {
		return 0
	}
	var conns []net.Conn
	stressWaitFor(t, "all proxy backend stream entries", func() bool {
		conns = conns[:0]
		server.mu.Lock()
		for _, e := range server.sessions {
			e.session.mu.Lock()
			for _, st := range e.session.streams {
				conns = append(conns, st.conn)
			}
			e.session.mu.Unlock()
		}
		server.mu.Unlock()
		return len(conns) == expected
	})
	for _, c := range conns {
		if err := stressSocketBuffer(c, size); err != nil {
			t.Fatal(err)
		}
	}
	return len(conns)
}

func stressInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, e := strconv.Atoi(v)
	if e != nil || n < 1 {
		t.Fatalf("invalid %s", name)
	}
	return n
}
func descriptorCount() int {
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		if entries, err := os.ReadDir(dir); err == nil {
			return len(entries)
		}
	}
	// macOS does not always allow directory enumeration of /dev/fd.
	if out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(os.Getpid()), "-Ff").Output(); err == nil {
		n := 0
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "f") {
				if _, err := strconv.Atoi(strings.TrimPrefix(line, "f")); err == nil {
					n++
				}
			}
		}
		return n
	}
	return -1
}
func networkMemoryDenied() int64 {
	if runtime.GOOS != "darwin" {
		return -1
	}
	out, err := exec.Command("netstat", "-m").Output()
	if err != nil {
		return -1
	}
	return parseNetworkMemoryDenied(string(out))
}

// netstat omits the denied line when the cumulative count is zero. Distinguish
// that valid zero from unavailable or unrecognizable command output.
func parseNetworkMemoryDenied(out string) int64 {
	valid := strings.Contains(out, "mbufs in use:") && strings.Contains(out, "KB allocated to network")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "requests for memory denied") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				return -1
			}
			n, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil || n < 0 {
				return -1
			}
			return n
		}
	}
	if valid {
		return 0
	}
	return -1
}

func currentRSS() int64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n * 1024
}

// Opt in: SGATE_STRESS=1 go test -run '^TestStressReconnectResources$' -v ./internal/tunnel
func TestStressReconnectResources(t *testing.T) {
	if os.Getenv("SGATE_STRESS") != "1" {
		t.Skip("set SGATE_STRESS=1 to run the resource stress test")
	}
	duplex := os.Getenv("SGATE_STRESS_DUPLEX") == "1"
	tlsMode := os.Getenv("SGATE_STRESS_TLS") == "1"
	lifecycle := os.Getenv("SGATE_STRESS_LIFECYCLE") == "1"
	defaultRetention := os.Getenv("SGATE_STRESS_DEFAULT_RETENTION") == "1"
	unconfirmedMode := os.Getenv("SGATE_STRESS_UNCONFIRMED")
	if unconfirmedMode != "" && (unconfirmedMode != "tls-timeout" && unconfirmedMode != "abandoned" && unconfirmedMode != "silent" && unconfirmedMode != "control" && unconfirmedMode != "fragmented" || !tlsMode || !duplex || !lifecycle || !defaultRetention) {
		t.Fatal("unconfirmed mode requires WSS, duplex, lifecycle and default retention; choose tls-timeout, abandoned, silent, control or fragmented")
	}
	var boundaryOutage time.Duration
	if v := os.Getenv("SGATE_STRESS_BOUNDARY_OUTAGE"); v != "" {
		var err error
		boundaryOutage, err = time.ParseDuration(v)
		if err != nil || boundaryOutage < 119*time.Second || boundaryOutage > 121*time.Second || !defaultRetention || !tlsMode || !duplex || !lifecycle {
			t.Fatal("boundary outage requires WSS, duplex, lifecycle, default retention and 119s..121s")
		}
	}
	var retentionOutage time.Duration
	if v := os.Getenv("SGATE_STRESS_RETENTION_OUTAGE"); v != "" {
		var err error
		retentionOutage, err = time.ParseDuration(v)
		if err != nil || retentionOutage <= 0 || retentionOutage >= 2*time.Minute || !defaultRetention || lifecycle {
			t.Fatal("retention outage requires default retention, 0 < outage < 2m, and no lifecycle mode")
		}
	}
	drainBudget := 30*time.Second + retentionOutage
	initialGoroutines := runtime.NumGoroutine()
	initialDescriptors := descriptorCount()
	clients := stressInt(t, "SGATE_STRESS_CLIENTS", 4)
	streams := stressInt(t, "SGATE_STRESS_STREAMS", 64)
	rounds := stressInt(t, "SGATE_STRESS_ROUNDS", 12)
	if lifecycle && (!duplex || rounds < 2) {
		t.Fatal("lifecycle stress requires independent duplex mode and at least two rounds")
	}
	payloadBytes := stressInt(t, "SGATE_STRESS_BYTES", 128*1024)
	minDuration := time.Duration(0)
	if v := os.Getenv("SGATE_STRESS_DURATION"); v != "" {
		var err error
		minDuration, err = time.ParseDuration(v)
		if err != nil {
			t.Fatal(err)
		}
	}
	report := stressReport{Clients: clients, StreamsPerClient: streams, PayloadBytes: payloadBytes, TLS: tlsMode}
	report.RetentionOutage = retentionOutage.String()
	report.BoundaryOutage = boundaryOutage.String()
	report.UnconfirmedMode = unconfirmedMode
	if duplex {
		report.SocketBufferBytes = 16 * 1024
	}
	if v := os.Getenv("SGATE_STRESS_SOCKET_BUFFER"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			t.Fatal("invalid SGATE_STRESS_SOCKET_BUFFER")
		}
		report.SocketBufferBytes = n
	}
	start := time.Now()
	report.IndependentDuplex = duplex
	recordInterval := 250 * time.Millisecond
	if duplex {
		recordInterval = 2 * time.Second
	}
	if v := os.Getenv("SGATE_STRESS_RECORD_INTERVAL"); v != "" {
		var err error
		recordInterval, err = time.ParseDuration(v)
		if err != nil || recordInterval <= 0 {
			t.Fatal("invalid SGATE_STRESS_RECORD_INTERVAL")
		}
	}
	report.RecordInterval = recordInterval.String()
	report.ApplicationDialTimeout = stressApplicationDialTimeout.String()
	barrierPrefix := os.Getenv("SGATE_STRESS_LOAD_BARRIER_PREFIX")
	report.LoadSynchronized = barrierPrefix != ""
	var cohortSettle time.Duration
	if v := os.Getenv("SGATE_STRESS_COHORT_SETTLE"); v != "" {
		var err error
		cohortSettle, err = time.ParseDuration(v)
		if err != nil || cohortSettle < 0 {
			t.Fatal("invalid cohort settle duration")
		}
	}
	report.CohortSettle = cohortSettle.String()
	var uploadSent, uploadReceived, downloadSent, downloadReceived atomic.Int64
	backendErrors := make(chan error, clients*streams*rounds)
	var backendCount, localCount, httpCount connectionCounter
	backendCount.socketBuffer, localCount.socketBuffer, httpCount.socketBuffer = report.SocketBufferBytes, report.SocketBufferBytes, report.SocketBufferBytes
	report.BackendHost = os.Getenv("SGATE_STRESS_BACKEND_HOST")
	if report.BackendHost == "" {
		report.BackendHost = "127.0.0.1"
	}
	if report.BackendHost != "127.0.0.1" && report.BackendHost != "::1" {
		t.Fatal("stress backend must use a loopback address")
	}
	report.ServerHost = os.Getenv("SGATE_STRESS_SERVER_HOST")
	if report.ServerHost == "" {
		report.ServerHost = "127.0.0.1"
	}
	if report.ServerHost != "127.0.0.1" && report.ServerHost != "::1" {
		t.Fatal("stress HTTP server must use a loopback address")
	}
	listen := func(counter *connectionCounter) net.Listener {
		host := "127.0.0.1"
		if counter == &backendCount {
			host = report.BackendHost
		} else if counter == &httpCount {
			host = report.ServerHost
		}
		l, e := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if e != nil {
			t.Fatal(e)
		}
		return countedListener{l, counter}
	}
	backend := listen(&backendCount)
	var backendWG sync.WaitGroup
	var backendConns sync.Map
	var applicationConns sync.Map
	var reportDisconnects atomic.Int64
	var lifecyclePeer atomic.Pointer[stressLifecycleProbe]
	var verifiedBytes atomic.Int64
	backendWG.Add(1)
	go func() {
		defer backendWG.Done()
		for {
			c, e := backend.Accept()
			if e != nil {
				return
			}
			backendConns.Store(c, true)
			backendWG.Add(1)
			go func() {
				defer backendWG.Done()
				defer c.Close()
				defer backendConns.Delete(c)
				if !duplex {
					_, _ = io.Copy(c, c)
					return
				}
				var hello [16]byte
				if _, err := io.ReadFull(c, hello[:]); err != nil {
					backendErrors <- err
					return
				}
				seed := binary.BigEndian.Uint64(hello[:8])
				if binary.BigEndian.Uint64(hello[8:]) == 0 && lifecycle {
					probe := lifecyclePeer.Load()
					if probe == nil {
						backendErrors <- fmt.Errorf("unexpected lifecycle probe")
						return
					}
					probe.backend(c, seed)
					return
				}
				end := time.Unix(0, int64(binary.BigEndian.Uint64(hello[8:])))
				c.SetDeadline(end.Add(drainBudget))
				if err := stressDuplex(c, seed, end, payloadBytes, true, &downloadSent, &uploadReceived, recordInterval); err != nil {
					backendErrors <- err
				}
			}()
		}
	}()
	opts := Options{BufferBytes: 256 * 1024, MaxStreams: streams + 16, Heartbeat: 5 * time.Second, DialTimeout: 2 * time.Second, WriteTimeout: 10 * time.Second, Logf: func(string, ...any) {}}
	if tlsMode {
		opts.DialTimeout, opts.HeartbeatTimeout = 5*time.Second, 30*time.Second
	}
	if os.Getenv("SGATE_STRESS_PROBE_TRACE") == "1" {
		opts.Logf = func(format string, args ...any) {
			if strings.HasPrefix(format, "tunnel disconnected:") {
				t.Logf("transport %s %s", time.Now().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
			}
		}
	}
	if os.Getenv("SGATE_STRESS_LOG") == "1" {
		opts.Logf = func(format string, args ...any) {
			t.Logf("%s %s", time.Now().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
		}
	}
	if v := os.Getenv("SGATE_STRESS_NOISE_INTERVAL"); v != "" {
		var err error
		opts.NoiseInterval, err = time.ParseDuration(v)
		if err != nil {
			t.Fatal(err)
		}
	}
	for name, dst := range map[string]*time.Duration{
		"SGATE_STRESS_NOISE_INTERVAL_MIN": &opts.NoiseIntervalMin,
		"SGATE_STRESS_NOISE_INTERVAL_MAX": &opts.NoiseIntervalMax,
	} {
		if v := os.Getenv(name); v != "" {
			var err error
			*dst, err = time.ParseDuration(v)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	opts.NoiseMinBytes = stressInt(t, "SGATE_STRESS_NOISE_MIN_BYTES", 1024)
	opts.NoiseMaxBytes = stressInt(t, "SGATE_STRESS_NOISE_MAX_BYTES", 10240)
	report.NoiseIntervalMin, report.NoiseIntervalMax = opts.NoiseIntervalMin.String(), opts.NoiseIntervalMax.String()
	report.NoiseMinBytes, report.NoiseMaxBytes = opts.NoiseMinBytes, opts.NoiseMaxBytes
	churnInterval, offlineDuration := 30*time.Millisecond, 10*time.Millisecond
	for name, dst := range map[string]*time.Duration{
		"SGATE_STRESS_CHURN_INTERVAL":   &churnInterval,
		"SGATE_STRESS_OFFLINE_DURATION": &offlineDuration,
	} {
		if v := os.Getenv(name); v != "" {
			var err error
			*dst, err = time.ParseDuration(v)
			if err != nil || *dst <= 0 {
				t.Fatalf("invalid %s", name)
			}
		}
	}
	key := bytes.Repeat([]byte{42}, 32)
	report.ChurnInterval, report.OfflineDuration = churnInterval.String(), offlineDuration.String()
	serverConfig := ServerConfig{Password: testPassword, Key: key, Target: backend.Addr().String(), SessionTTL: 10 * time.Second, Options: opts}
	report.SessionRetention = "10s"
	if defaultRetention {
		serverConfig.SessionTTL = 0
		report.SessionRetention = "2m0s"
	}
	var clientOfflineTimeout time.Duration
	if v := os.Getenv("SGATE_STRESS_UNCONFIRMED_TTL"); v != "" {
		var err error
		serverConfig.SessionTTL, err = time.ParseDuration(v)
		if err != nil || serverConfig.SessionTTL < 6*time.Second || unconfirmedMode == "" {
			t.Fatal("invalid unconfirmed test retention")
		}
		report.SessionRetention = serverConfig.SessionTTL.String()
		clientOfflineTimeout = serverConfig.SessionTTL
	}
	server, e := NewServer(serverConfig)
	if e != nil {
		t.Fatal(e)
	}
	var offline atomic.Bool
	var probeNonces sync.Map
	var serverTLS *tls.Config
	startHTTP := func(service *Server, addr string) *httptest.Server {
		h := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, probeAllowed := probeNonces.Load(r.Header.Get("X-Sgate-Nonce"))
			if offline.Load() && !probeAllowed {
				http.Error(w, "offline", 503)
				return
			}
			service.ServeHTTP(w, r)
		}))
		h.Listener.Close()
		if addr == "" {
			h.Listener = listen(&httpCount)
		} else {
			l, err := net.Listen("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			h.Listener = countedListener{l, &httpCount}
		}
		if tlsMode {
			if serverTLS != nil {
				h.TLS = serverTLS.Clone()
			}
			h.StartTLS()
			if serverTLS == nil {
				serverTLS = h.TLS.Clone()
			}
		} else {
			h.Start()
		}
		return h
	}
	httpServer := startHTTP(server, "")
	var clientTLS *tls.Config
	if tlsMode {
		roots := x509.NewCertPool()
		roots.AddCert(httpServer.Certificate())
		clientTLS = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	transportURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/tunnel"
	var relay *weakRelay
	if os.Getenv("SGATE_STRESS_WEAKNET") == "1" {
		profile := weakProfile{delay: 150 * time.Millisecond, jitter: 100 * time.Millisecond, lossPercent: 15, retransmit: 200 * time.Millisecond}
		for name, dst := range map[string]*time.Duration{
			"SGATE_STRESS_WEAK_DELAY":      &profile.delay,
			"SGATE_STRESS_WEAK_JITTER":     &profile.jitter,
			"SGATE_STRESS_WEAK_RETRANSMIT": &profile.retransmit,
		} {
			if v := os.Getenv(name); v != "" {
				var err error
				*dst, err = time.ParseDuration(v)
				if err != nil || *dst < 0 {
					t.Fatalf("invalid %s", name)
				}
			}
		}
		profile.lossPercent = stressInt(t, "SGATE_STRESS_WEAK_LOSS_PERCENT", 15)
		if profile.lossPercent > 100 {
			t.Fatal("loss percent must not exceed 100")
		}
		report.WeakNetwork = true
		report.WeakRelayBatchBytes = chunkSize
		report.WeakDelay, report.WeakJitter, report.WeakRetransmit = profile.delay.String(), profile.jitter.String(), profile.retransmit.String()
		report.WeakLossPercent = profile.lossPercent
		relay = newWeakRelayDialTimeout(t, httpServer.Listener.Addr().String(), opts.DialTimeout)
		report.WeakRelayDialTimeout = opts.DialTimeout.String()
		relay.profile.Store(&profile)
		transportURL = "ws://" + relay.listener.Addr().String() + "/tunnel"
		if tlsMode {
			transportURL = "wss://" + relay.listener.Addr().String() + "/tunnel"
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var clientWG sync.WaitGroup
	addrs := make([]string, clients)
	states := make([]*clientSessions, clients)
	reconnectMin, reconnectMax := 5*time.Millisecond, 25*time.Millisecond
	for name, dst := range map[string]*time.Duration{"SGATE_STRESS_RECONNECT_MIN": &reconnectMin, "SGATE_STRESS_RECONNECT_MAX": &reconnectMax} {
		if value := os.Getenv(name); value != "" {
			var err error
			*dst, err = time.ParseDuration(value)
			if err != nil || *dst <= 0 {
				t.Fatalf("invalid %s", name)
			}
		}
	}
	report.ReconnectMin, report.ReconnectMax = reconnectMin.String(), reconnectMax.String()
	for i := range addrs {
		l := listen(&localCount)
		addrs[i] = l.Addr().String()
		c, err := NewClient(ClientConfig{Password: testPassword, Key: key, URL: transportURL, TLSConfig: clientTLS, OfflineTimeout: clientOfflineTimeout, ReconnectMin: reconnectMin, ReconnectMax: reconnectMax, Options: opts})
		if err != nil {
			t.Fatal(err)
		}
		state := &clientSessions{}
		states[i] = state
		clientWG.Add(1)
		go func() {
			defer clientWG.Done()
			if err := c.run(ctx, l, state); err != nil {
				t.Error(err)
			}
		}()
	}
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			applicationConns.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
			cancel()
			clientWG.Wait()
			server.Close()
			httpServer.Close()
			if relay != nil {
				relay.close()
			}
			backend.Close()
			backendConns.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
			backendWG.Wait()
		})
	}
	defer cleanup()
	attached := func() bool {
		server.mu.Lock()
		if len(server.sessions) != clients {
			server.mu.Unlock()
			return false
		}
		for _, e := range server.sessions {
			e.session.mu.Lock()
			ready := e.inUse && e.session.active != nil
			e.session.mu.Unlock()
			if !ready {
				server.mu.Unlock()
				return false
			}
		}
		server.mu.Unlock()
		for _, state := range states {
			state.mu.Lock()
			ready := false
			if state.current != nil {
				state.current.mu.Lock()
				ready = state.current.active != nil && !state.current.closed
				state.current.mu.Unlock()
			}
			state.mu.Unlock()
			if !ready {
				return false
			}
		}
		return true
	}
	stressWaitFor(t, "all stress transports", attached)
	livePath := os.Getenv("SGATE_STRESS_LIVE_REPORT")
	sample := func(phase string, round int, collect bool) stressSample {
		if collect {
			debug.FreeOSMemory()
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		p := stressSample{SampleTimeUnixNano: time.Now().UnixNano(), NetworkMemoryDenied: -1, Phase: phase, Round: round, Milliseconds: time.Since(start).Milliseconds(), HeapBytes: m.HeapAlloc, HeapInuseBytes: m.HeapInuse, HeapIdleBytes: m.HeapIdle, HeapReleasedBytes: m.HeapReleased, StackBytes: m.StackInuse, SysBytes: m.Sys, Goroutines: runtime.NumGoroutine(), FileDescriptors: -1, BackendConnections: backendCount.active.Load(), LocalConnections: localCount.active.Load(), HTTPConnections: httpCount.active.Load()}
		if phase != "load" && phase != "unconfirmed-progress" {
			p.FileDescriptors = descriptorCount()
		}
		if collect {
			p.RSSBytes = currentRSS()
			p.NetworkMemoryDenied = networkMemoryDenied()
			buf := make([]byte, 8<<20)
			n := runtime.Stack(buf, true)
			if n == len(buf) {
				t.Fatal("writer census stack buffer too small")
			}
			p.TransportWriters = bytes.Count(buf[:n], []byte(".(*session).writeTransport("))
		} else {
			p.TransportWriters = -1
		}
		if relay != nil {
			p.RelayConnections, p.RelayQueuedBytes = relay.active.Load(), relay.queued.Load()
			p.Retransmissions, p.RelayAccepted = relay.losses.Load(), relay.accepted.Load()
			p.RelayWrites, p.RelayCoalesced, p.RelayWaitTimers = relay.writeCalls.Load(), relay.coalesced.Load(), relay.waitTimers.Load()
		}
		server.mu.Lock()
		p.Sessions = len(server.sessions)
		p.Nonces = len(server.nonces)
		for _, e := range server.sessions {
			e.session.mu.Lock()
			p.Streams += len(e.session.streams)
			p.PendingBytes += e.session.pendingBytes
			p.ServerNoiseSent += e.session.noiseSent
			p.ServerNoiseReceived += e.session.noiseReceived
			e.session.mu.Unlock()
		}
		server.mu.Unlock()
		for _, state := range states {
			state.mu.Lock()
			if st := state.current; st != nil {
				st.mu.Lock()
				if !st.closed {
					p.ClientSessions++
				}
				p.ClientStreams += len(st.streams)
				p.ClientPendingBytes += st.pendingBytes
				p.ClientNoiseSent += st.noiseSent
				p.ClientNoiseReceived += st.noiseReceived
				st.mu.Unlock()
			}
			state.mu.Unlock()
		}
		// Census after collection: in particular the initial server in-use flag
		// can precede the client's transport/writer goroutines under heavy load.
		p.Goroutines = runtime.NumGoroutine()
		report.Samples = append(report.Samples, p)
		if livePath != "" {
			b, err := json.Marshal(p)
			if err == nil {
				err = os.WriteFile(livePath+".tmp", b, 0644)
			}
			if err == nil {
				err = os.Rename(livePath+".tmp", livePath)
			}
			if err != nil {
				t.Fatalf("live stress sample: %v", err)
			}
		}
		if collect {
			t.Logf("%s round=%d heap=%.2fMiB rss=%.2fMiB g=%d fd=%d backend=%d local=%d http=%d sessions=%d streams=%d pending=%d nonces=%d heap_idle=%d heap_released=%d", phase, round, float64(p.HeapBytes)/(1<<20), float64(p.RSSBytes)/(1<<20), p.Goroutines, p.FileDescriptors, p.BackendConnections, p.LocalConnections, p.HTTPConnections, p.Sessions, p.Streams, p.PendingBytes, p.Nonces, p.HeapIdleBytes, p.HeapReleasedBytes)
			t.Logf("client streams=%d pending=%d; noise server tx/rx=%d/%d client tx/rx=%d/%d", p.ClientStreams, p.ClientPendingBytes, p.ServerNoiseSent, p.ServerNoiseReceived, p.ClientNoiseSent, p.ClientNoiseReceived)
			t.Logf("kernel network memory denied=%d", p.NetworkMemoryDenied)
			if relay != nil {
				t.Logf("relay connections=%d queued=%d retransmissions=%d accepted=%d; transport writers=%d", p.RelayConnections, p.RelayQueuedBytes, p.Retransmissions, p.RelayAccepted, p.TransportWriters)
			}
		}
		return p
	}
	base := sample("baseline", 0, true)
	if base.TransportWriters != 2*clients {
		stressWaitFor(t, "all initial transport writers before baseline", func() bool {
			base = sample("baseline", 0, true)
			return base.TransportWriters == 2*clients
		})
	}
	if path := os.Getenv("SGATE_STRESS_REPORT"); path != "" {
		defer func() {
			report.Disconnects = reportDisconnects.Load()
			b, err := json.MarshalIndent(report, "", "  ")
			if err == nil {
				err = os.MkdirAll(filepath.Dir(path), 0755)
			}
			if err == nil {
				err = os.WriteFile(path, append(b, '\n'), 0644)
			}
			if err != nil {
				t.Error(err)
			}
		}()
	}
	defer func() {
		if len(report.Samples) > 0 && report.Samples[len(report.Samples)-1].Phase == "closed" {
			return
		}
		// Failed load assertions must still expose post-shutdown resources.
		// This runs before report serialization and preserves the failure.
		cleanup()
		waitFor(t, "failed stress sockets closed", func() bool {
			return backendCount.active.Load() == 0 && localCount.active.Load() == 0 && httpCount.active.Load() == 0
		})
		waitFor(t, "failed stress goroutines stopped", func() bool { return runtime.NumGoroutine() <= initialGoroutines+4 })
		closed := sample("closed", report.CompletedRounds, true)
		if closed.TransportWriters != 0 || closed.RelayConnections != 0 || closed.RelayQueuedBytes != 0 ||
			closed.Sessions != 0 || closed.Streams != 0 || closed.PendingBytes != 0 || closed.Nonces != 0 ||
			closed.ClientSessions != 0 || closed.ClientStreams != 0 || closed.ClientPendingBytes != 0 {
			t.Error("resources survived failed stress shutdown")
		}
	}()
	settle := func(phase string, round int) {
		if cohortSettle == 0 {
			return
		}
		// Completed cohorts leave kernel TIME_WAIT entries. Wait between cohorts,
		// never inside a live load/retention phase or by reducing active streams.
		sample(phase, round, false)
		t.Logf("%s: waiting %s for completed TCP cohort TIME_WAIT; active target remains %d", phase, cohortSettle, clients*streams)
		timer := time.NewTimer(cohortSettle)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-timer.C:
		}
	}
	for round := 1; round <= rounds; round++ {
		roundEnd := time.Now().Add(minDuration / time.Duration(rounds))
		var conns []net.Conn
		for _, addr := range addrs {
			for i := 0; i < streams; i++ {
				c, err := net.DialTimeout("tcp", addr, stressApplicationDialTimeout)
				if err != nil {
					t.Fatal(err)
				}
				if report.SocketBufferBytes > 0 {
					if err := stressSocketBuffer(c, report.SocketBufferBytes); err != nil {
						c.Close()
						t.Fatal(err)
					}
				}
				deadline := time.Now().Add(drainBudget)
				if roundEnd.Add(drainBudget).After(deadline) {
					deadline = roundEnd.Add(drainBudget)
				}
				_ = c.SetDeadline(deadline)
				applicationConns.Store(c, true)
				conns = append(conns, c)
			}
		}
		stressWaitFor(t, "all stress backend streams", func() bool { return backendCount.active.Load() == int64(clients*streams) })
		report.BackendSocketsBound += stressBoundBackendSockets(t, server, report.SocketBufferBytes, clients*streams)
		if barrierPrefix != "" {
			for _, c := range conns {
				_ = c.SetDeadline(time.Time{})
			}
			if err := waitStressLoadBarrier(ctx, barrierPrefix, round, clients*streams, time.Minute); err != nil {
				t.Fatal(err)
			}
		}
		sample("connected", round, false)
		// Admission is warmup, not part of the offered-load duration. Slow local
		// allocation must not shorten traffic or remove disconnect injections.
		ready := time.Now()
		roundEnd = ready.Add(minDuration / time.Duration(rounds))
		for _, c := range conns {
			_ = c.SetDeadline(roundEnd.Add(drainBudget))
		}
		report.LoadWindows = append(report.LoadWindows, stressLoadWindow{round, clients * streams, ready.UnixNano(), roundEnd.UnixNano()})
		abort := make(chan struct{})
		churnDone := make(chan struct{})
		var retainedClients, retainedRemotes []*session
		go func() {
			defer close(churnDone)
			ticker := time.NewTicker(churnInterval)
			defer ticker.Stop()
			for {
				select {
				case <-abort:
					return
				case <-ticker.C:
				}
				offline.Store(true)
				server.mu.Lock()
				for _, e := range server.sessions {
					if retentionOutage > 0 {
						retainedRemotes = append(retainedRemotes, e.session)
					}
					e.session.mu.Lock()
					if e.session.active != nil {
						e.session.active.Close()
					}
					e.session.mu.Unlock()
				}
				server.mu.Unlock()
				reportDisconnects.Add(1)
				if retentionOutage > 0 {
					for _, state := range states {
						state.mu.Lock()
						retainedClients = append(retainedClients, state.current)
						state.mu.Unlock()
					}
					stressHoldRetainedOutage(t, retentionOutage, abort, retainedClients, retainedRemotes)
					offline.Store(false)
					return
				}
				timer := time.NewTimer(offlineDuration)
				select {
				case <-abort:
					timer.Stop()
					offline.Store(false)
					return
				case <-timer.C:
				}
				offline.Store(false)
			}
		}()
		errors := make(chan error, len(conns))
		var workers sync.WaitGroup
		for i, c := range conns {
			workers.Add(1)
			go func(i int, c net.Conn) {
				defer workers.Done()
				defer c.Close()
				defer applicationConns.Delete(c)
				if duplex {
					var hello [16]byte
					seed := uint64(round)<<32 | uint64(i)
					binary.BigEndian.PutUint64(hello[:8], seed)
					binary.BigEndian.PutUint64(hello[8:], uint64(roundEnd.UnixNano()))
					if _, err := io.Copy(c, bytes.NewReader(hello[:])); err != nil {
						errors <- err
						return
					}
					if err := stressDuplex(c, seed, roundEnd, payloadBytes, false, &uploadSent, &downloadReceived, recordInterval); err != nil {
						errors <- err
					}
					return
				}
				p := bytes.Repeat([]byte{byte(round), byte(i), byte(i >> 8), 0x5a}, (payloadBytes+3)/4)[:payloadBytes]
				for {
					if err := exchange(c, p); err != nil {
						errors <- err
						return
					}
					verifiedBytes.Add(int64(len(p)))
					if !time.Now().Before(roundEnd) {
						break
					}
					time.Sleep(recordInterval)
				}
				if err := c.(*net.TCPConn).CloseWrite(); err != nil {
					errors <- err
					return
				}
				var b [1]byte
				if n, err := c.Read(b[:]); n != 0 || err != io.EOF {
					errors <- fmt.Errorf("extra data or missing EOF: %d %v", n, err)
				}
			}(i, c)
		}
		done := make(chan struct{})
		go func() { workers.Wait(); close(done) }()
		ticker := time.NewTicker(100 * time.Millisecond)
		lastProgress, progressAt, dumped := int64(-1), time.Now(), false
	run:
		for {
			select {
			case <-done:
				break run
			case <-ticker.C:
				sample("load", round, false)
				if duplex {
					progress := uploadReceived.Load() + downloadReceived.Load()
					// A deliberately blocked retention window is not a stalled
					// healthy transport. Start a fresh progress budget on recovery.
					if retentionOutage > 0 && offline.Load() {
						progressAt = time.Now()
					}
					if progress != lastProgress {
						lastProgress, progressAt = progress, time.Now()
					}
					if !dumped && (time.Since(progressAt) > 2*recordInterval+5*time.Second || os.Getenv("SGATE_STRESS_SNAPSHOT") == "1" && time.Since(start) > 3*time.Second) {
						dumped = true
						if path := os.Getenv("SGATE_STRESS_REPORT"); path != "" {
							buf := make([]byte, 8<<20)
							n := runtime.Stack(buf, true)
							_ = os.WriteFile(path+".stacks", buf[:n], 0644)
							t.Logf("saved diagnostic stacks to %s.stacks", path)
						}
					}
				}
			}
		}
		ticker.Stop()
		close(abort)
		<-churnDone
		if retentionOutage > 0 {
			stressCheckRetainedIdentity(t, states, server, retainedClients, retainedRemotes)
			report.RetainedSessionContinuity = true
		}
		for _, c := range conns {
			c.Close()
		}
		conns = nil
		close(errors)
		for err := range errors {
			t.Errorf("round %d: %v", round, err)
		}
		if t.Failed() {
			for i := 0; i < 8; i++ {
				select {
				case err := <-backendErrors:
					t.Logf("backend error: %v", err)
				default:
					return
				}
			}
			return
		}
		waitFor(t, "all transports resumed", attached)
		waitFor(t, "all stress streams released", func() bool {
			if backendCount.active.Load() != 0 || localCount.active.Load() != 0 {
				return false
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			for _, e := range server.sessions {
				e.session.mu.Lock()
				clean := len(e.session.streams) == 0 && e.session.pendingBytes == 0 && e.session.pending == nil
				e.session.mu.Unlock()
				if !clean {
					return false
				}
			}
			return true
		})
		for _, state := range states {
			state.mu.Lock()
			st := state.current
			state.mu.Unlock()
			waitFor(t, "client streams and replay cache released", func() bool {
				st.mu.Lock()
				defer st.mu.Unlock()
				return len(st.streams) == 0 && st.pendingBytes == 0 && st.pending == nil
			})
		}
		if relay != nil {
			waitFor(t, "old weak relay connections released", func() bool { return relay.active.Load() == int64(clients) })
		}
		idle := sample("idle", round, true)
		if idle.TransportWriters > 2*clients {
			t.Fatal("transport writer count grew beyond one per peer")
		}
		if relay != nil && idle.RelayConnections > int64(clients) {
			t.Fatal("old relay connections survived recovery")
		}
		if duplex {
			if uploadSent.Load() != uploadReceived.Load() || downloadSent.Load() != downloadReceived.Load() {
				t.Fatal("independent directional byte counts differ")
			}
			if idle.ClientStreams != 0 || idle.ClientPendingBytes != 0 {
				t.Fatal("client resources were not released")
			}
		}
		select {
		case err := <-backendErrors:
			t.Fatalf("backend: %v", err)
		default:
		}
		if idle.Goroutines > base.Goroutines+16 {
			t.Fatalf("goroutines did not return to baseline: %d -> %d", base.Goroutines, idle.Goroutines)
		}
		if base.FileDescriptors >= 0 && idle.FileDescriptors > base.FileDescriptors+8 {
			t.Fatalf("descriptors did not return to baseline: %d -> %d", base.FileDescriptors, idle.FileDescriptors)
		}
		if idle.HeapBytes > base.HeapBytes+16*1024*1024 {
			t.Fatalf("retained heap grew beyond allowance: %d -> %d", base.HeapBytes, idle.HeapBytes)
		}
		report.CompletedRounds = round
		report.VerifiedBytesPerDirection = verifiedBytes.Load()
		if duplex {
			report.VerifiedUploadBytes = uploadReceived.Load()
			report.VerifiedDownloadBytes = downloadReceived.Load()
		}
		if round == rounds && (opts.NoiseInterval > 0 || opts.NoiseIntervalMin > 0) && (idle.ServerNoiseReceived == 0 || idle.ClientNoiseReceived == 0) {
			t.Fatal("stress did not actually receive padding in both directions")
		}
		if lifecycle && round < rounds {
			settle("cohort-settle-before-lifecycle", round)
			kind := "restart"
			if round%2 == 0 {
				kind = "expiry"
			}
			if defaultRetention {
				kind = "expiry"
			}
			if boundaryOutage > 0 {
				kind = "boundary"
			}
			if unconfirmedMode != "" {
				kind = unconfirmedMode
			}
			stressLifecyclePhase(t, stressLifecycleContext{
				kind: kind, round: round, streams: streams, addrs: addrs, states: states,
				server: &server, httpServer: &httpServer, serverConfig: serverConfig,
				startHTTP: startHTTP, offline: &offline, peer: &lifecyclePeer,
				backendCount: &backendCount, localCount: &localCount, socketBuffer: report.SocketBufferBytes,
				applicationConns: &applicationConns, attached: attached, sample: sample, relay: relay,
				boundaryOutage: boundaryOutage, report: &report,
				probeNonces: &probeNonces, probeURL: transportURL, probeTLS: clientTLS,
			})
			if kind == "boundary" {
				report.InterruptedStreams = report.BoundaryInterruptedStreams
			} else {
				report.InterruptedStreams += clients * streams
			}
			if kind == "restart" {
				report.ServerRestarts++
			} else if kind == "expiry" {
				report.SessionExpiries++
			}
			settle("cohort-settle-after-lifecycle", round)
		}

	}
	report.Disconnects = reportDisconnects.Load()
	var oldSessions []*session
	server.mu.Lock()
	for _, e := range server.sessions {
		oldSessions = append(oldSessions, e.session)
	}
	server.mu.Unlock()
	for _, state := range states {
		state.mu.Lock()
		oldSessions = append(oldSessions, state.current)
		state.mu.Unlock()
	}
	cleanup()
	for _, s := range oldSessions {
		if s != nil {
			assertReleased(t, "stress transport and its timers stopped", s)
		}
	}
	waitFor(t, "stress sockets closed", func() bool {
		return backendCount.active.Load() == 0 && localCount.active.Load() == 0 && httpCount.active.Load() == 0
	})
	waitFor(t, "stress goroutines stopped", func() bool { return runtime.NumGoroutine() <= initialGoroutines+4 })
	closed := sample("closed", report.CompletedRounds, true)
	if closed.TransportWriters != 0 || closed.RelayConnections != 0 || closed.RelayQueuedBytes != 0 {
		t.Fatal("transport writer, relay connection or queued bytes survived shutdown")
	}
	if relay != nil && closed.Retransmissions == 0 {
		t.Fatal("no simulated packet retransmission was injected")
	}
	if closed.ClientSessions != 0 || closed.ClientStreams != 0 || closed.ClientPendingBytes != 0 || closed.Sessions != 0 || closed.Streams != 0 || closed.PendingBytes != 0 {
		t.Fatal("session resources survived shutdown")
	}
	if initialDescriptors >= 0 && closed.FileDescriptors > initialDescriptors+4 {
		t.Fatalf("descriptors leaked after shutdown: %d -> %d", initialDescriptors, closed.FileDescriptors)
	}
	if duplex {
		t.Logf("completed: rounds=%d disconnects=%d verified upload=%d download=%d", report.CompletedRounds, report.Disconnects, report.VerifiedUploadBytes, report.VerifiedDownloadBytes)
	} else {
		t.Logf("completed: rounds=%d disconnects=%d bytes/direction=%d", report.CompletedRounds, report.Disconnects, report.VerifiedBytesPerDirection)
	}
}
