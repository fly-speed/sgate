// Package aclgin adapts the official gin-service router template for sgate HTTPS.
package aclgin

import (
	mastergin "github.com/acl-dev/gin-service"
	"github.com/gin-gonic/gin"
	"net"
	"net/http"
	"sync"
)

var setup sync.Once

// HTTPHandler uses gin-service's public service/router structure. sgate owns
// http.Server.Serve and TLS wrapping so the existing timeouts, byte accounting
// and explicit shutdown of hijacked WebSockets remain in force.
func HTTPHandler(listener net.Listener, handler http.Handler) http.Handler {
	setup.Do(func() { gin.SetMode(gin.ReleaseMode) })
	engine := gin.New()
	engine.RedirectTrailingSlash = false
	engine.RedirectFixedPath = false
	service := mastergin.GinService{Servers: []*mastergin.GinServ{{Listener: listener, Engine: engine}}}
	service.GET("/*sgatePath", gin.WrapH(handler))
	service.NoRoute(gin.WrapH(handler))
	return engine
}
