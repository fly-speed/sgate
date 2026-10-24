package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mastergin "github.com/acl-dev/gin-service"
	"github.com/gin-gonic/gin"
)

func routeFixture(t *testing.T, path string, management http.Handler) (*mastergin.GinService, string, *atomic.Value) {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	service, err := mastergin.Init("127.0.0.1:0", mastergin.WithContext(context.Background()), mastergin.WithManaged(false))
	if err != nil {
		t.Fatal(err)
	}
	var done chan struct{}
	t.Cleanup(func() {
		service.Close()
		if done != nil {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("framework Run did not exit")
			}
		}
	})
	activePath := &atomic.Value{}
	activePath.Store(path)
	_, err = service.Configure(mastergin.ServerConfig{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Route-Owner", "proxy")
		if r.URL.Path != activePath.Load().(string) {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "proxy")
	})})
	if err != nil {
		t.Fatal(err)
	}
	setRoute(service, path, management)
	done = make(chan struct{})
	go func() { service.Run(); close(done) }()
	return service, "http://" + service.Addresses()[0], activePath
}

func routeRequest(t *testing.T, method, target string, want int, owner string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	request, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != want || response.Header.Get("X-Route-Owner") != owner {
		t.Fatalf("%s %s: got %d owner=%q, want %d owner=%q",
			method, target, response.StatusCode, response.Header.Get("X-Route-Owner"), want, owner)
	}
}

func TestRoutesKeepManagementSeparateAndSupportPathChanges(t *testing.T) {
	management := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Route-Owner", "admin")
		switch {
		case r.URL.Path == "/api/status":
			w.WriteHeader(200)
		case r.URL.Path == "/api/config" && r.Method == "POST":
			w.WriteHeader(405)
		case strings.HasPrefix(r.URL.Path, "/api/"):
			w.WriteHeader(401)
		default:
			http.NotFound(w, r)
		}
	})
	service, base, path := routeFixture(t, "/tunnel", management)
	registered := map[string]bool{}
	for _, route := range service.Servers[0].Engine.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	for _, endpoint := range []string{"GET /tunnel", "GET /admin/api/status", "POST /admin/api/restart-service", "PUT /admin/api/config"} {
		if !registered[endpoint] {
			t.Fatal("route missing from central registration:", endpoint)
		}
	}
	routeRequest(t, "GET", base+"/admin", 307, "")
	routeRequest(t, "POST", base+"/admin", 307, "")
	routeRequest(t, "GET", base+"/admin/api/status", 200, "admin")
	routeRequest(t, "POST", base+"/admin/api/restart-service", 401, "admin")
	routeRequest(t, "PUT", base+"/admin/api/config", 401, "admin")
	routeRequest(t, "POST", base+"/admin/api/config", 405, "admin")
	routeRequest(t, "GET", base+"/admin/missing", 404, "admin")
	routeRequest(t, "GET", base+"/admin-other/api/status", 404, "proxy")
	routeRequest(t, "GET", base+"/tunnel", 200, "proxy")
	path.Store("/new-tunnel")
	routeRequest(t, "GET", base+"/tunnel", 404, "proxy")
	routeRequest(t, "GET", base+"/new-tunnel", 200, "proxy")
	// Changing only the trailing slash must not redirect to the stale route.
	path.Store("/tunnel/")
	routeRequest(t, "GET", base+"/tunnel/", 200, "proxy")
	routeRequest(t, "GET", base+"/tunnel", 404, "proxy")
}

func TestRoutesWithoutManagementPreserveLiteralProxyPaths(t *testing.T) {
	for _, path := range []string{"/admin", "/literal/:name", "/literal/*", "/"} {
		t.Run(path, func(t *testing.T) {
			_, base, _ := routeFixture(t, path, nil)
			routeRequest(t, "GET", base+path, 200, "proxy")
			routeRequest(t, "GET", base+"/unmatched", 404, "proxy")
		})
	}
}
