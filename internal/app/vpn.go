package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"sgate/internal/vpn"
)

type vpnHealth struct {
	Generation   string       `json:"generation,omitempty"`
	Enabled      bool         `json:"enabled"`
	Healthy      bool         `json:"healthy"`
	State        string       `json:"state"`
	Role         string       `json:"role,omitempty"`
	Mode         string       `json:"mode,omitempty"`
	Device       string       `json:"device,omitempty"`
	Address      string       `json:"address,omitempty"`
	Network      string       `json:"network,omitempty"`
	ClientIP     string       `json:"client_ip,omitempty"`
	LastError    string       `json:"last_error,omitempty"`
	QueueMaxAge  string       `json:"queue_max_age"`
	NoiseRate    int          `json:"noise_rate"`
	NoiseTraffic bool         `json:"noise_traffic"`
	NoiseIdle    bool         `json:"noise_idle"`
	Stats        vpn.Snapshot `json:"stats"`
}
type vpnDevice interface {
	vpn.Device
	Name() string
}
type vpnWorker struct {
	mu     sync.Mutex
	health vpnHealth
	stats  *vpn.Stats
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (v *vpnWorker) snapshot() vpnHealth {
	if v == nil {
		return vpnHealth{Healthy: true, State: "disabled"}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	h := v.health
	h.Stats = v.stats.Snapshot()
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
	i.startVPNWithDevice(ctx, p, func(name string) (vpnDevice, error) { return vpn.OpenTUN(name) })
}
func (i *instance) startVPNWithDevice(ctx context.Context, p *vpn.Prepared, open func(string) (vpnDevice, error)) {
	if p == nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	v := &vpnWorker{stats: &vpn.Stats{}, cancel: cancel, done: make(chan struct{}), health: vpnHealth{Generation: strconv.FormatInt(time.Now().UnixNano(), 10), Enabled: true, QueueMaxAge: p.Config.QueueMaxAge.String(), NoiseRate: p.Config.Noise.Rate, NoiseTraffic: p.Config.Noise.Traffic, NoiseIdle: p.Config.Noise.Idle, State: "starting", Role: p.Role, Mode: p.Mode, Network: p.Config.Network.String()}}
	i.vpn = v
	go func() {
		defer close(v.done)
		err := v.run(ctx, p, open)
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
func (v *vpnWorker) run(ctx context.Context, p *vpn.Prepared, open func(string) (vpnDevice, error)) error {
	config, network, err := p.Resolve(ctx)
	if err != nil {
		return err
	}
	config.Stats = v.stats
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
		v.health.ClientIP = config.ClientIP.String()
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
		v.health.Address = server.Addr().String()
		v.health.State = "listening"
		v.health.Healthy = true
		v.mu.Unlock()
		return server.Serve(ctx)
	}
	if p.Manage {
		config.Connected = func() (func(), error) { return vpn.ApplyNetwork(network) }
	}
	v.mu.Lock()
	v.health.State = "connecting"
	v.mu.Unlock()
	return vpn.RunClient(ctx, config, device)
}
func (c *controller) replaceVPN(next definition) {
	old := c.current.vpn
	if old == nil && next.vpn == nil {
		return
	}
	if old != nil && next.vpn != nil && old.Fingerprint == next.vpn.Fingerprint {
		h := c.worker.vpn.snapshot()
		if h.State != "failed" && h.State != "stopped" {
			return
		}
	}
	c.worker.vpn.stop()
	c.worker.vpn = nil
	c.worker.startVPN(c.ctx, next.vpn)
}

func (c *controller) workerVPNStatus() vpnHealth {
	if c.worker == nil {
		return (*vpnWorker)(nil).snapshot()
	}
	if c.worker.vpn == nil && c.current.vpn != nil && c.role == "client" {
		p := c.current.vpn
		return vpnHealth{Enabled: true, Healthy: true, State: "stopped", Role: "client", Mode: p.Mode, Network: p.Config.Network.String(), QueueMaxAge: p.Config.QueueMaxAge.String(), NoiseRate: p.Config.Noise.Rate, NoiseTraffic: p.Config.Noise.Traffic, NoiseIdle: p.Config.Noise.Idle}
	}
	h := c.worker.vpn.snapshot()
	if c.role == "client" {
		h.Role = "client"
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
		return nil
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
	c.worker.startVPN(c.ctx, c.current.vpn)
	return nil
}
