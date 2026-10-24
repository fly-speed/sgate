package main

import (
	"time"

	mastergin "github.com/acl-dev/gin-service"
	"sgate/internal/app"
)

// 仅适配生命周期接口，让共用的 app 包和客户端不依赖 Gin。
type frameworkService struct{ *mastergin.GinService }

func (s frameworkService) Managed() bool { return !s.Alone }

func (s frameworkService) Configure(c app.HTTPConfig) (app.HTTPWorker, error) {
	return s.GinService.Configure(mastergin.ServerConfig{
		Addresses:         c.Addresses,
		TLSConfig:         c.TLS,
		Handler:           c.Handler,
		WrapListener:      c.WrapListener,
		OnStop:            c.OnStop,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	})
}
