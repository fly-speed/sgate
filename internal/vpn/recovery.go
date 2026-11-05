package vpn

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
)

var ErrClientRecoveryPending = errors.New("上次 VPN 的网络设置尚待清理，请先在实时概览确认清理")

// Only settings on the physical network survive the VPN device closing.
type clientRecoveryState struct {
	Platform            string
	Endpoint, Gateway   netip.Addr
	Device              string
	Index               int
	DNSService          string
	PreviousDNS, VPNDNS []string
}

type clientRecovery struct {
	path  string
	lock  *os.File
	state clientRecoveryState
}

func openClientRecovery(path string) (*clientRecovery, error) {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = lockRecoveryFile(lock); err != nil {
		lock.Close()
		return nil, err
	}
	r := &clientRecovery{path: path, lock: lock, state: clientRecoveryState{Platform: runtime.GOOS}}
	return r, nil
}

func ClientRecoveryPending(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Lstat(path)
	return !os.IsNotExist(err)
}

func (r *clientRecovery) save() error {
	if !r.state.Endpoint.IsValid() && r.state.DNSService == "" {
		err := os.Remove(r.path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	data, err := json.Marshal(r.state)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(r.path), ".vpn-recovery-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return err
	}
	return os.Rename(file.Name(), r.path)
}

func (r *clientRecovery) rememberRoute(endpoint, gateway netip.Addr, device string, index int) error {
	r.state.Endpoint, r.state.Gateway, r.state.Device, r.state.Index = endpoint, gateway, device, index
	return r.save()
}

func (r *clientRecovery) rememberDNS(service string, before, after []string) error {
	r.state.DNSService, r.state.PreviousDNS, r.state.VPNDNS = service, before, after
	return r.save()
}

func (r *clientRecovery) clearRoute() error {
	r.state.Endpoint = netip.Addr{}
	return r.save()
}

func (r *clientRecovery) clearDNS() error {
	r.state.DNSService = ""
	return r.save()
}

func RecoverClientNetwork(path string) error {
	return recoverClientNetwork(path, recoverClientDNS, recoverClientRoute)
}

func recoverClientNetwork(path string, recoverDNS, recoverRoute func(clientRecoveryState) error) error {
	if !ClientRecoveryPending(path) {
		return nil
	}
	r, err := openClientRecovery(path)
	if err != nil {
		return err
	}
	defer r.lock.Close()
	data, err := readRecoveryRecord(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) > 16384 || json.Unmarshal(data, &r.state) != nil || r.state.Platform != runtime.GOOS {
		return errors.New("无法识别上次 VPN 的网络恢复记录")
	}
	var dnsErr, routeErr error
	if r.state.DNSService != "" {
		if dnsErr = recoverDNS(r.state); dnsErr == nil {
			dnsErr = r.clearDNS()
		}
	}
	if r.state.Endpoint.IsValid() {
		if routeErr = recoverRoute(r.state); routeErr == nil {
			routeErr = r.clearRoute()
		}
	}
	if err := errors.Join(dnsErr, routeErr); err != nil {
		return fmt.Errorf("清理上次 VPN 的网络设置失败：%w", err)
	}
	return r.save()
}

func readRecoveryRecord(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("VPN 网络恢复记录的文件类型、权限或所有者无效")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) || !trustedRecoveryFile(f, opened) {
		return nil, errors.New("读取时 VPN 网络恢复记录发生变化")
	}
	return io.ReadAll(io.LimitReader(f, 16385))
}

// ApplyManagedClientNetwork holds the recovery lock through network cleanup.
func ApplyManagedClientNetwork(o NetworkOptions, path string) (func(), error) {
	if path == "" {
		return ApplyNetwork(o)
	}
	r, err := openClientRecovery(path)
	if err != nil {
		return nil, err
	}
	if ClientRecoveryPending(path) {
		r.lock.Close()
		return nil, ErrClientRecoveryPending
	}
	o.recovery = r
	undo, err := ApplyNetwork(o)
	if err != nil {
		r.lock.Close()
		return nil, err
	}
	return func() { defer r.lock.Close(); undo() }, nil
}

func recordCleanup(undo, clear func() error) func() error {
	return func() error {
		if err := undo(); err != nil {
			return err
		}
		return clear()
	}
}
