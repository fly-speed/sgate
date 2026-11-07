package app

import (
	"errors"
	"net"
	"strings"

	"sgate/internal/admin"
)

func (d definition) clientRoutes(singleAddress string) []clientProxy {
	if len(d.proxies) > 0 {
		return d.proxies
	}
	if d.client == nil {
		return nil
	}
	var routes []clientProxy
	seen := map[string]bool{}
	for _, configured := range strings.Split(d.listen, ",") {
		configured = strings.TrimSpace(configured)
		if _, port, _ := net.SplitHostPort(configured); port == "0" {
			configured = singleAddress
		}
		address, err := net.ResolveTCPAddr("tcp", configured)
		if err != nil {
			return nil
		}
		_, port, _ := net.SplitHostPort(address.String())
		if !seen[port] {
			routes = append(routes, clientProxy{Listen: address.String(), Server: d.client.URL, Addr: d.client.Addr, AllowReverse: d.client.AllowReverse, port: port, config: *d.client})
			seen[port] = true
		}
	}
	return routes
}

func sameClientAddresses(a, b string) bool {
	parse := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, v := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' }) {
			m[strings.ReplaceAll(v, "|", ":")] = true
		}
		return m
	}
	x, y := parse(a), parse(b)
	if len(x) != len(y) {
		return false
	}
	for k := range x {
		if !y[k] {
			return false
		}
	}
	return true
}

// Apply only mapping targets/listeners. Pending credentials, certificates and
// other non-live fields remain pending; all routes retain effective CLI values.
func (c *controller) replaceClientMappings(next definition) error {
	if c.worker.proxies == nil {
		return errors.New("客户端映射运行器不可用")
	}
	settings, err := processSettings(c.path)
	if err != nil {
		return err
	}
	if address, ok := settings["master_service"]; ok && !sameClientAddresses(address, next.listen) {
		return errors.New("master_service 必须与生效的映射监听地址一致")
	}
	if c.isManaged() && !sameClientAddresses(next.listen, c.current.listen) {
		if c.servicePath == "" {
			return errors.New("缺少 acl_master 服务文件，无法自动重启")
		}
		if err := checkManagedMappingPorts(next.listen, c.tcp.Addresses()); err != nil {
			return err
		}
		c.pendingListeners = next.listen
		return admin.ErrMappingServiceRestart
	}
	base := c.current.client
	if len(c.current.proxies) > 0 {
		base = &c.current.proxies[0].config
	}
	current := c.current
	current.listen = next.listen
	current.proxies = nil
	current.client = nil
	if len(next.proxies) > 0 {
		current.proxies = append([]clientProxy(nil), next.proxies...)
		for n := range current.proxies {
			cfg := *base
			cfg.URL, cfg.Addr = next.proxies[n].config.URL, next.proxies[n].config.Addr
			cfg.AllowReverse = next.proxies[n].AllowReverse
			// Mapping credentials are live; unrelated common credential edits stay pending.
			cfg.Username, cfg.Password = c.current.commonUsername, c.current.commonPassword
			if current.proxies[n].Username != nil {
				cfg.Username = *current.proxies[n].Username
			}
			if current.proxies[n].Password != nil {
				cfg.Password = *current.proxies[n].Password
			}
			current.proxies[n].config = cfg
		}
	} else {
		cfg := *base
		cfg.URL, cfg.Addr = next.client.URL, next.client.Addr
		cfg.AllowReverse = c.current.commonAllowReverse // Do not apply pending single-proxy security edits.
		cfg.Username, cfg.Password = c.current.commonUsername, c.current.commonPassword
		current.client = &cfg
	}
	routes := current.withLiveSettings(c.worker.live).clientRoutes(current.listen)
	for _, route := range routes {
		if route.port == "0" {
			return errors.New("在线映射更新需要固定的非零 TCP 端口")
		}
	}
	var update func() error
	if !sameClientAddresses(next.listen, c.current.listen) {
		updater, ok := c.tcp.(interface{ UpdateAddresses(string) error })
		if !ok {
			return errors.New("go-service 不支持在线监听更新，请升级客户端")
		}
		update = func() error { return updater.UpdateAddresses(next.listen) }
	}
	if err := c.worker.proxies.replace(routes, update); err != nil {
		return err
	}
	c.current = current
	c.pendingListeners = ""
	if value, ok := settings["master_service"]; ok {
		if c.controls == nil {
			c.controls = map[string]string{}
		}
		c.controls["master_service"] = value
	}
	return nil
}

// New ports can be checked without stopping the running process. Inherited
// ports are still held by acl_master and cannot be rebound until stop/start.
func checkManagedMappingPorts(next string, inherited []string) error {
	ports := map[string]bool{}
	for _, address := range inherited {
		_, port, _ := net.SplitHostPort(address)
		ports[port] = true
	}
	var probes []net.Listener
	defer func() {
		for _, l := range probes {
			l.Close()
		}
	}()
	for _, address := range strings.Split(next, ",") {
		_, port, _ := net.SplitHostPort(address)
		if ports[port] {
			continue
		}
		l, err := net.Listen("tcp", address)
		if err != nil {
			return err
		}
		probes = append(probes, l)
	}
	return nil
}
