package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"sgate/internal/cli"
	"sgate/internal/vpn"
)

type vpnHealth struct {
	Clients         []vpn.ClientSnapshot `json:"clients,omitempty"`
	Generation      string               `json:"generation,omitempty"`
	RecoveryPending bool                 `json:"recovery_pending,omitempty"`
	Enabled         bool                 `json:"enabled"`
	Healthy         bool                 `json:"healthy"`
	State           string               `json:"state"`
	Role            string               `json:"role,omitempty"`
	Mode            string               `json:"mode,omitempty"`
	Device          string               `json:"device,omitempty"`
	Address         string               `json:"address,omitempty"`
	Transport       string               `json:"transport,omitempty"`
	UDPAddress      string               `json:"udp_address,omitempty"`
	TCPAddress      string               `json:"tcp_address,omitempty"`
	Network         string               `json:"network,omitempty"`
	ClientIP        string               `json:"client_ip,omitempty"`
	LastError       string               `json:"last_error,omitempty"`
	QueueMaxAge     string               `json:"queue_max_age"`
	NoiseRate       int                  `json:"noise_rate"`
	NoiseTraffic    bool                 `json:"noise_traffic"`
	NoiseIdle       bool                 `json:"noise_idle"`
	Stats           vpn.Snapshot         `json:"stats"`
}
type vpnDevice interface {
	vpn.Device
	Name() string
}
type vpnWorker struct {
	restartFromSaved bool // Protected by the controller mutex.
	server           *vpn.Server
	serverAccounts   *vpn.Config
	mu               sync.Mutex
	health           vpnHealth
	stats            *vpn.Stats
	live             *vpn.LiveSettings
	cancel           context.CancelFunc
	done             chan struct{}
	once             sync.Once
}

func (v *vpnWorker) snapshot() vpnHealth {
	if v == nil {
		return vpnHealth{Healthy: true, State: "disabled"}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	h := v.health
	h.Stats = v.stats.Snapshot()
	if v.server != nil {
		h.Clients = v.server.Clients()
	}
	if v.live != nil {
		live := v.live.Snapshot()
		h.NoiseRate = live.Noise.Rate
		h.NoiseTraffic = live.Noise.Traffic
		h.NoiseIdle = live.Noise.Idle
		h.QueueMaxAge = live.QueueMaxAge.String()
	}
	if h.State == "connecting" || h.State == "connected" || h.State == "reconnecting" {
		h.Healthy = h.Stats.Active > 0
		if h.Healthy {
			h.State = "connected"
		} else if h.Stats.Connections > 0 {
			h.State = "reconnecting"
		} else {
			h.State = "connecting"
		}
		if h.Healthy {
			h.LastError = ""
		}
	}
	return h
}
func (v *vpnWorker) stop() {
	if v == nil {
		return
	}
	v.once.Do(v.cancel)
	<-v.done
}
func (i *instance) startVPN(ctx context.Context, p *vpn.Prepared) {
	if i.openTUN != nil {
		i.startVPNWithDevice(ctx, p, i.openTUN)
		return
	}
	i.startVPNWithDevice(ctx, p, func(name string) (vpnDevice, error) { return vpn.OpenTUN(name) })
}
func (i *instance) startVPNWithDevice(ctx context.Context, p *vpn.Prepared, open func(string) (vpnDevice, error)) {
	if p == nil {
		return
	}
	var identityErr error
	if p.Role == "client" {
		if i.vpnIdentity == "" {
			i.vpnIdentity, identityErr = vpn.NewClientIdentity()
		}
		copy := *p
		copy.Config = p.Config
		copy.Config.ClientID = i.vpnIdentity
		p = &copy
	}
	ctx, cancel := context.WithCancel(ctx)
	live, liveErr := vpn.NewLiveSettings(vpn.LiveFromConfig(p.Config))
	v := &vpnWorker{live: live, stats: &vpn.Stats{}, cancel: cancel, done: make(chan struct{}), health: vpnHealth{Generation: strconv.FormatInt(time.Now().UnixNano(), 10), Enabled: true, Transport: p.Config.Transport, TCPAddress: p.Config.TCPListen, QueueMaxAge: p.Config.QueueMaxAge.String(), NoiseRate: p.Config.Noise.Rate, NoiseTraffic: p.Config.Noise.Traffic, NoiseIdle: p.Config.Noise.Idle, State: "starting", Role: p.Role, Mode: p.Mode, Network: p.Config.Network.String()}}
	i.vpn = v
	if liveErr == nil {
		liveErr = identityErr
	}
	if liveErr != nil {
		v.health.State = "failed"
		v.health.LastError = liveErr.Error()
		close(v.done)
		return
	}
	go func() {
		defer close(v.done)
		err := v.run(ctx, p, open, i.vpnRecoveryPath)
		v.mu.Lock()
		defer v.mu.Unlock()
		v.health.Healthy = false
		if ctx.Err() != nil {
			v.health.State = "stopped"
		} else {
			v.health.State = "failed"
			v.health.LastError = fmt.Sprint(err)
			log.Printf("VPN stopped: %v", err)
		}
	}()
}
func (v *vpnWorker) run(ctx context.Context, p *vpn.Prepared, open func(string) (vpnDevice, error), recoveryPath string) error {
	config, network, err := p.Resolve(ctx)
	if err != nil {
		return err
	}
	config.Stats = v.stats
	config.Live = v.live
	v.mu.Lock()
	v.health.Transport = config.Transport
	if v.health.Transport == "" {
		v.health.Transport = "quic"
	}
	if p.Role == "server" {
		transports := []string{}
		if config.Address != "" {
			transports = append(transports, "quic")
		}
		if config.UDPListen != "" {
			transports = append(transports, "udp")
			v.health.UDPAddress = config.UDPListen
		}
		if config.TCPListen != "" {
			transports = append(transports, "tcp", "wss")
		}
		v.health.Transport = strings.Join(transports, ",")
	}
	v.mu.Unlock()
	config.Logf = func(format string, args ...any) {
		message := fmt.Sprintf(format, args...)
		log.Print(message)
		v.mu.Lock()
		v.health.LastError = message
		v.mu.Unlock()
	}
	network.Logf = config.Logf
	if err := ctx.Err(); err != nil {
		return err
	}
	device, err := open(p.TUN)
	if err != nil {
		return fmt.Errorf("open VPN TUN (administrator privileges required): %w", err)
	}
	defer device.Close()
	network.Interface = device.Name()
	v.mu.Lock()
	v.health.Device = device.Name()
	v.health.Address = config.Address
	if p.Role == "client" {
		if config.ClientIP.IsValid() {
			v.health.ClientIP = config.ClientIP.String()
		}
	}
	v.mu.Unlock()
	if p.Role == "server" {
		if p.Manage {
			undo, err := vpn.ApplyNetwork(network)
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
		v.mu.Lock()
		if v.serverAccounts != nil {
			server.UpdateAccounts(*v.serverAccounts)
			v.serverAccounts = nil
		}
		v.server = server
		if server.UDPAddr() != nil {
			v.health.UDPAddress = server.UDPAddr().String()
		}
		v.health.Address = server.Addr().String()
		if server.TCPAddr() != nil {
			v.health.TCPAddress = server.TCPAddr().String()
		}
		v.health.State = "listening"
		v.health.Healthy = true
		v.mu.Unlock()
		return server.Serve(ctx)
	}
	config.Assigned = func(ip netip.Addr) {
		network.ClientIP = ip
		v.mu.Lock()
		v.health.ClientIP = ip.String()
		v.mu.Unlock()
	}
	if p.Manage {
		config.Connected = func() (func(), error) {
			undo, err := vpn.ApplyManagedClientNetwork(network, recoveryPath)
			v.mu.Lock()
			v.health.RecoveryPending = errors.Is(err, vpn.ErrClientRecoveryPending)
			v.mu.Unlock()
			return undo, err
		}
	}
	v.mu.Lock()
	v.health.State = "connecting"
	v.mu.Unlock()
	return vpn.RunClient(ctx, config, device)
}
func (c *controller) replaceVPN(next definition) {
	old := c.current.vpn
	if c.role == "client" && c.worker.vpn == nil {
		return
	}
	if old == nil && next.vpn == nil {
		return
	}
	if old != nil && next.vpn != nil && old.Fingerprint == next.vpn.Fingerprint {
		h := c.worker.vpn.snapshot()
		if h.State != "failed" && h.State != "stopped" {
			if c.worker.vpn != nil && c.worker.vpn.live != nil {
				c.worker.vpn.live.Update(vpn.LiveFromConfig(next.vpn.Config))
			}
			return
		}
	}
	c.worker.vpn.stop()
	c.worker.vpn = nil
	c.worker.startVPN(c.ctx, next.vpn)
}

func (c *controller) vpnRecoveryPath() string {
	if c.role != "client" || c.path == "" {
		return ""
	}
	return c.path + ".vpn-recovery"
}

func (c *controller) workerVPNStatus() vpnHealth {
	if c.worker == nil {
		return (*vpnWorker)(nil).snapshot()
	}
	h := c.worker.vpn.snapshot()
	if c.role == "client" {
		if c.worker.vpn == nil && c.current.vpn != nil {
			p := c.current.vpn
			h = vpnHealth{Enabled: true, Healthy: true, State: "stopped", Mode: p.Mode, Transport: p.Config.Transport, Network: p.Config.Network.String(), QueueMaxAge: p.Config.QueueMaxAge.String(), NoiseRate: p.Config.Noise.Rate, NoiseTraffic: p.Config.Noise.Traffic, NoiseIdle: p.Config.Noise.Idle}
		}
		h.Role = "client"
		if h.State == "stopped" || h.State == "disabled" || h.State == "failed" {
			h.RecoveryPending = vpn.ClientRecoveryPending(c.vpnRecoveryPath())
		}
	}
	return h
}

// controlVPN serializes network ownership with apply and process shutdown.
// Starting always uses the applied configuration, never unsaved form values.
func (c *controller) controlVPN(start bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.role != "client" {
		return errors.New("仅客户端支持手动控制 VPN")
	}
	if c.ctx.Err() != nil || c.restartPending {
		return errors.New("客户端正在退出或重启，无法操作 VPN")
	}
	if c.worker == nil {
		return errors.New("客户端未运行")
	}
	if !start {
		c.worker.vpn.stop() // Joins RunClient, including network cleanup and TUN close.
		c.worker.vpn = nil
		return vpn.RecoverClientNetwork(c.vpnRecoveryPath())
	}
	if c.current.vpn == nil {
		return errors.New("请先将 vpn-mode 设置为 both 或 only，并保存及应用配置")
	}
	if v := c.worker.vpn; v != nil {
		h := v.snapshot()
		if h.State != "failed" && h.State != "stopped" {
			return nil
		}
		v.stop()
	}
	if vpn.ClientRecoveryPending(c.vpnRecoveryPath()) {
		return vpn.ErrClientRecoveryPending
	}
	c.worker.vpnRecoveryPath = c.vpnRecoveryPath()
	c.worker.startVPN(c.ctx, c.current.vpn)
	return nil
}

func (c *controller) prepareSavedVPN() (*vpn.Prepared, error) {
	fs := cli.CloneFlags(c.original)
	if err := cli.Parse(fs, c.args); err != nil {
		return nil, err
	}
	return vpn.Prepare(c.role, fs)
}

func (c *controller) vpnRestartPending() (pending, retry bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	saved, err := c.prepareSavedVPN()
	if err != nil {
		return false, false, err
	}
	running := c.current.vpn
	if saved == nil || running == nil {
		return saved != running, false, nil
	}
	retry = c.worker != nil && c.worker.vpn != nil && c.worker.vpn.restartFromSaved && c.workerVPNStatus().State == "failed"
	return saved.Fingerprint != running.Fingerprint || retry, retry, nil
}

// restartVPN applies saved VPN settings without applying pending proxy edits.
func (c *controller) restartVPN() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil || c.restartPending || c.worker == nil {
		return errors.New("服务正在停止或重启，无法重启 VPN")
	}
	prepared, err := c.prepareSavedVPN()
	if err != nil {
		return err
	}
	only := prepared != nil && prepared.Mode == "only"
	if only != c.current.vpnOnly {
		return errors.New("vpn-mode 改变了代理运行模式，请使用全局“保存并应用”切换模式")
	}

	started := c.worker.vpn != nil
	c.worker.vpn.stop()
	c.worker.vpn = nil
	c.current.vpn = prepared
	if prepared != nil && (c.role != "client" || started) {
		c.worker.startVPN(c.ctx, prepared)
		c.worker.vpn.restartFromSaved = true
	}
	return nil
}
