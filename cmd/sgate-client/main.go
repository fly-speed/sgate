package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	master "github.com/acl-dev/go-service"
	"sgate/internal/aclservice"
	"sgate/internal/app"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// 与 go-echod 一样：初始化 TCP 服务、注册回调，再进入框架 Run。
// 管理 HTTP 服务由 proxy.Configure 在独立协程中启动。
func run() error {
	options, err := parseClientOptions()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	proxy, err := app.NewClient(ctx, flag.CommandLine, os.Args[1:], options.adminConfig)
	if err != nil {
		return err
	}
	defer proxy.Close()
	service, err := master.TcpServiceInit(proxy.Listen(), master.WithTCPContext(ctx), master.WithTCPManaged(aclservice.Enabled()))
	if err != nil {
		return err
	}
	defer service.Close()
	service.AcceptHandler = proxy.Accept
	service.CloseHandler = proxy.Closed
	if err = proxy.Configure(frameworkService{service}); err != nil {
		return err
	}
	service.Run()
	return proxy.Err()
}

type frameworkService struct{ *master.TcpService }

func (s frameworkService) Configure(address string) (net.Listener, error) {
	return s.TcpService.Configure(address)
}

func (s frameworkService) ActiveConnections() int { return master.ConnCountCur() }
