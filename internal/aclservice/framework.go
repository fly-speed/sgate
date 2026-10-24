package aclservice

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"

	master "github.com/acl-dev/go-service"
)

// Enabled keeps the historical standalone invocation working without -alone.
// acl_master supplies -f/-t/-s; -alone explicitly selects standalone operation.
func Enabled() bool { return !master.Alone && master.Configure != "" }

type Runtime struct {
	Context context.Context
	Pool    *Pool
	cancel  context.CancelFunc
	done    chan struct{}
}

func Open(parent context.Context) (*Runtime, error) {
	if master.ServiceType != "sock" && master.ServiceType != "inet" && master.ServiceType != "unix" {
		return nil, fmt.Errorf("unsupported acl_master service type %q", master.ServiceType)
	}
	if master.SocketCount < 1 || master.SocketCount > 256 {
		return nil, errors.New("acl_master listener count must be between 1 and 256")
	}
	if master.Chroot || master.Unprivileged {
		return nil, errors.New(("sgate requires master_chroot=n and master_unpriv=n; run acl_master under the " +
			"intended service account"))
	}
	closed, err := closeInheritedSockets(master.SocketCount)
	if err != nil {
		return nil, err
	}
	if closed != 0 {
		log.Printf("acl_master closed %d unrelated inherited sockets", closed)
	}
	// Prepare and GetListeners are the official go-service TCP template's
	// configuration/descriptor API. Do not call its global Run/Wait monitor:
	// sgate needs a joined, cancellable lifecycle and repeatable listener leases.
	master.Prepare()
	if master.AppConf.GetInt("master_maxproc") != 1 || master.AppConf.GetInt("master_prefork") != 1 {
		return nil, errors.New(("sgate requires master_maxproc=1 and master_prefork=1; tunnel sessions and Web " +
			"management state belong to one process"))
	}
	if master.AppConf.GetBool("master_status_notify") {
		return nil, errors.New("sgate requires master_status_notify=0")
	}
	if master.AppReusePort {
		return nil, errors.New("sgate requires master_reuseport=no so acl_master owns the listening sockets")
	}
	listeners, e := master.GetListeners()
	if e != nil {
		return nil, e
	}
	cleanup := func() {
		for _, l := range listeners {
			l.Close()
		}
	}
	if len(listeners) != master.SocketCount {
		cleanup()
		return nil, errors.New("not all acl_master listener descriptors could be inherited")
	}
	f := os.NewFile(5, "acl_master status")
	if f == nil {
		cleanup()
		return nil, errors.New("missing acl_master status descriptor 5")
	}
	state, e := net.FileConn(f)
	f.Close()
	if e != nil {
		cleanup()
		return nil, fmt.Errorf("acl_master status descriptor: %w", e)
	}
	ctx, cancel := context.WithCancel(parent)
	r := &Runtime{Context: ctx, cancel: cancel, done: make(chan struct{})}
	r.Pool = NewPool(listeners, cancel)
	go func() {
		defer close(r.done)
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			var b [1]byte
			_, _ = state.Read(b[:])
			cancel()
		}()
		<-ctx.Done()
		state.Close()
		r.Pool.Close()
		<-readDone
	}()
	log.Printf("acl_master inherited listeners: %v", r.Pool.Addresses())
	return r, nil
}
func (r *Runtime) Close() {
	r.cancel()
	<-r.done
}
