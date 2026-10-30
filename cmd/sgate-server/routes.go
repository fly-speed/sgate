package main

import (
	"net/http"
	"strings"
	"time"

	mastergin "github.com/acl-dev/gin-service"
	"github.com/gin-gonic/gin"
)

// setRoute 集中注册服务端 HTTP 入口，所有框架监听使用相同的路由。
func setRoute(service *mastergin.GinService, proxyPath string, management http.Handler) {
	// Handler 从请求上下文取得所属 Worker，旧连接不会串到新一代代理。
	proxy := gin.WrapH(service.Handler())
	admin := managementHandler(management)
	for _, server := range service.Servers {
		routes := server.Engine
		if management != nil {
			routes.Any("/admin", redirectManagement)
			// 复用管理处理器的登录、来源和 CSRF 校验，入口方法和路径在此列明。
			group := routes.Group("/admin")
			group.GET("/", admin)
			group.GET("/assets/app.css", admin)
			group.GET("/assets/app.js", admin)
			group.GET("/assets/i18n.js", admin)
			group.GET("/api/status", admin)
			group.POST("/api/setup", admin)
			group.POST("/api/login", admin)
			group.POST("/api/logout", admin)
			group.PUT("/api/account", admin)
			group.GET("/api/tunnel-accounts", admin)
			group.PUT("/api/tunnel-accounts", admin)
			group.GET("/api/stats", admin)
			group.GET("/api/config", admin)
			group.PUT("/api/config", admin)
			group.POST("/api/apply", admin)
			group.POST("/api/restart-service", admin)
		}
		// 默认 /tunnel，也支持配置路径；冒号和星号在这里按字面处理，不作为 Gin 参数。
		if !strings.ContainsAny(proxyPath, ":*") {
			routes.GET(proxyPath, proxy)
		}
		// 配置应用可改变隧道路径，不修改运行中的 Gin 路由树；由当前代理校验新路径。
		routes.NoRoute(func(c *gin.Context) {
			// 保留管理端对未知接口/方法的 404、405，不把管理请求转入代理。
			if management != nil && c.Request.URL.Path == "/admin" {
				redirectManagement(c)
			} else if management != nil && strings.HasPrefix(c.Request.URL.Path, "/admin/") {
				admin(c)
			} else {
				proxy(c)
			}
		})
	}
}

func redirectManagement(c *gin.Context) {
	c.Redirect(http.StatusTemporaryRedirect, "/admin/")
}

func managementHandler(handler http.Handler) gin.HandlerFunc {
	if handler == nil {
		return nil
	}
	handler = http.StripPrefix("/admin", handler)
	return func(c *gin.Context) {
		control := http.NewResponseController(c.Writer)
		_ = control.SetReadDeadline(time.Now().Add(15 * time.Second))
		_ = control.SetWriteDeadline(time.Now().Add(15 * time.Second))
		defer control.SetReadDeadline(time.Time{})
		defer control.SetWriteDeadline(time.Time{})
		handler.ServeHTTP(c.Writer, c.Request)
	}
}
