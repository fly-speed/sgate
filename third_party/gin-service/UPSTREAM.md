 gin-service

基于 gin 开发的服务器模板，可以与 acl_master 服务管理框架深度集成。

## 一、安装

```shell
go install github.com/acl-dev/gin-service@v1.0.1
```

## 二、使用

### 2.1 简单示例

下面是一个简单的使用封装了 gin 服务框架可以与 acl_master 服务管理框架融合的简单示例：
```go
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"github.com/acl-dev/gin-service"
	"github.com/acl-dev/go-service"
	"github.com/gin-gonic/gin"
)

var (
	addresses = flag.String(
		"listen",
		"127.0.0.1:8088",
		"listen addresses in alone mode",
	)
	debugMode = flag.Bool(
		"debug",
		false,
		"If running gin server in debug mode",
	)
)

func main()  {
	flag.Parse()

	if !*debugMode {
		// 设置生产环境下的运行模式。
		gin.SetMode(gin.ReleaseMode)
	}

	// 1. 创建服务对象，内部可以包含多个HTTP服务监听对象，在 acl_master 框架下，
	// 内部会忽略此处所输入的地址，而是从 acl_master 继承监听句柄。
	service, err := master_gin.Init(*addresses, onStop)
	if err != nil {
		fmt.Println("Init master gin service failed:", err)
		return
	}

	// For debug: 设置客户端连接建立时的回调函数。
	service.AcceptHandler = func(conn net.Conn) {
		fmt.Printf("Connect from %s\r\n", conn.RemoteAddr())
	}

	// For debug: 设置客户端连接断开时的回调函数。
	service.CloseHandler = func(conn net.Conn) {
		fmt.Printf("Disconnect from %s\r\n", conn.RemoteAddr())
	}

	// For debug: 应用可以根据自身需要从配置文件中取得所设置的配置项。
	fmt.Printf("ServiceType=%s, test_src=%s, test_bool=%t\r\n",
		master.ServiceType, master.AppConf.GetString("test_src"),
		master.AppConf.GetBool("test_bool"))

	// 2. 设置 HTTP 服务路由。
	setRoute(*service)
	fmt.Println("Listen and running ...")

	// 3. 启动 HTTP 服务过程
	service.Run()
}

func ginWrap(f func(w http.ResponseWriter, r *http.Request)) gin.HandlerFunc {
	return func(context *gin.Context) {
		f(context.Writer, context.Request)
	}
}

func onTest(w http.ResponseWriter, _ *http.Request) {
	_, _ = fmt.Fprintf(w, "test: Hello World!\r\n")
}

// setRoute 设置 HTTP 服务路由。
func setRoute(service master_gin.GinService) {
	service.Engine.GET("/", func(context *gin.Context) {
		context.String(200, "hello world!\r\n")
	})
	service.Engine.GET("/test", ginWrap(onTest))

	// 也可以采用以下方法设置 HTTP 服务路由
	// 针对每一个 HTTP 服务监听对象设置 HTTP 服务路由。
	/*
	for _, s := range service.Servers {
		s.Engine.GET("/", func(context *gin.Context) {
			context.String(200, "hello world!\r\n")
		})
		s.Engine.GET("/test", ginWrap(onTest))
	}
	*/
}

// onStop 进程退出时的回调函数。
func onStop(bool) {
	fmt.Println("The process stopped!")
}
```

在这个例子中，首先需要导入以下三个模块：
```go
"github.com/acl-dev/gin-service"
"github.com/acl-dev/go-service"
"github.com/gin-gonic/gin"
```
其中：
- `go-service` 模块是相对独立（没有第三方依赖）的服务模板，可以与 `acl_master` 服务管理框架无缝整合；
- `gin-service` 封装了 `go-service` 及 `gin` 框架，可以达到既方便使用 `gin` 框架，又可以与 `acl_master` 整合的目的；
- `gin` 为功能强大的 Web 服务框架。

编译：
```shell
go build -o gin-server
```

手工模式下运行：
```shell
./gin-server -alone
```

### 2.2、将 gin-server 服务程序部署在 acl_master 框架下
#### 2.2.1 部署 acl_master 服务管理框架
首先需要从 `https://github.com/acl-dev/acl` 或 `https://gitee.com/acl-dev/acl` 下载 acl 工程，然后编译安装，过程如下：
```
#cd acl; make
#cd disk/master; ./setup.sh /opt/soft/acl-master
#cd /opt/soft/acl-master/sh; ./start.sh
```
上面过程便完成了编译、安装及启动 acl_master 服务管理框架的过程。
如果您使用 CentOS 操作系统，还可以通过下面过程来完成（即：生成 acl_master RPM 包，然后安装该 RPM 包即可）：
```
#cd packaging; make
#cd x86_64; rpm -ivh acl-master*.rpm
```
当 RPM 安装后 acl_master 服务管理程序会自动启动。

#### 2.2.2 部署 gin-server 服务程序至 acl_master 框架下
首先下载 go-service 软件包并编译其中的服务示例，然后安装这些服务程序：

```
#go install github.com/acl-dev/gin-service@v1.0.0
#cd $GOPATH/src/github.com/acl-dev/gin-service/examples/
#(cd gin-server; go get; go build; ./setup.sh /opt/soft/gin-server)
#/opt/soft/gin-server/bin/start.sh
```

最后运行 **`acl_master`** 服务框架中的管理工具来查看由 **`acl_master`** 管理的服务：
```shell
#/opt/soft/acl-master/bin/master_ctl -a list
```

结果显示如下：

```
status  service                                         type    proc    owner   conf
200     |87, |88, |89, gin-server.sock            4       2      root    /opt/soft/gin-server/conf/gin-server.cf
```

可以使用 `curl` 工具测试一下 gin-server 服务，如下：
```
# curl http://127.0.0.1:8888/test
test: hello world!
```

在上面由 master_ctl -a list 显示的管理内容中可以看到 gin-server 的运行身份为 `root`，需要想要 gin-server 运行在 `nobody` 身份下，则需要以下修改：
- 将 gin-server 目录修改为 nobody 属主：`chown -R nobody:nobody /opt/soft/gin-server`；
- 修改 `/opt/soft/gin-server/conf/gin-server.cf` 配置文件，修改其中的配置项：
  - master_args = -u
  - master_owner = nobody
- 重新启动 gin-server 服务
  - /opt/soft/gin-server/bin/stop.sh
  - /opt/soft/gin-server/bin/start.sh

然后再运行
```shell
#/opt/soft/acl-master/bin/master_ctl -a list
```
结果显示如下：

```
status  service                                         type    proc    owner   conf
200     |87, |88, |89, gin-server.sock            4       2      nobody    /opt/soft/gin-server/conf/gin-server.cf
```

- `ownedConn.NetConn` exposes the underlying connection for transport counter attribution, including under TLS, without transferring lifecycle ownership.
