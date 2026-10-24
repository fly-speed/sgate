package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	mastergin "github.com/acl-dev/gin-service"
	"github.com/gin-gonic/gin"
	"sgate/internal/aclservice"
	"sgate/internal/app"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// 独立启动和 acl_master 托管共用 Init → setRoute → Run。
func run() error {
	adminPath, err := parseOptions(os.Args[1:])
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	proxy, err := app.NewServer(ctx, flag.CommandLine, os.Args[1:], adminPath)
	if err != nil {
		return err
	}
	defer proxy.Close()

	gin.SetMode(gin.ReleaseMode)
	service, err := mastergin.Init(proxy.Listen(), mastergin.WithContext(ctx), mastergin.WithManaged(aclservice.Enabled()))
	if err != nil {
		return err
	}
	defer service.Close()
	service.AcceptHandler = func(c net.Conn) { log.Printf("gin-service accepted %s", c.RemoteAddr()) }
	service.CloseHandler = func(c net.Conn) { log.Printf("gin-service closed %s", c.RemoteAddr()) }
	if err = proxy.Configure(frameworkService{service}); err != nil {
		return err
	}

	setRoute(service, proxy.ProxyPath(), proxy.ManagementHandler())
	service.Run()
	return proxy.Err()
}
