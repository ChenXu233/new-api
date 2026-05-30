package router

import (
	"embed"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-contrib/gzip"
	"github.com/gin-contrib/static"
	"github.com/gin-gonic/gin"
)

// ThemeAssets holds the embedded frontend assets for both themes.
type ThemeAssets struct {
	DefaultBuildFS   embed.FS
	DefaultIndexPage []byte
	ClassicBuildFS   embed.FS
	ClassicIndexPage []byte
}

func SetWebRouter(router *gin.Engine, assets ThemeAssets) {
	// 开发模式：代理到 Rsbuild dev server（支持 HMR）
	if devProxy := os.Getenv("FRONTEND_DEV_PROXY"); devProxy != "" {
		setDevProxyRouter(router, devProxy)
		return
	}

	defaultFS := common.EmbedFolder(assets.DefaultBuildFS, "web/default/dist")
	classicFS := common.EmbedFolder(assets.ClassicBuildFS, "web/classic/dist")
	themeFS := common.NewThemeAwareFS(defaultFS, classicFS)

	router.Use(gzip.Gzip(gzip.DefaultCompression))
	router.Use(middleware.GlobalWebRateLimit())
	router.Use(middleware.Cache())
	router.Use(static.Serve("/", themeFS))
	router.NoRoute(func(c *gin.Context) {
		c.Set(middleware.RouteTagKey, "web")
		if strings.HasPrefix(c.Request.RequestURI, "/v1") || strings.HasPrefix(c.Request.RequestURI, "/api") || strings.HasPrefix(c.Request.RequestURI, "/assets") {
			controller.RelayNotFound(c)
			return
		}
		c.Header("Cache-Control", "no-cache")
		if common.GetTheme() == "classic" {
			c.Data(http.StatusOK, "text/html; charset=utf-8", assets.ClassicIndexPage)
		} else {
			c.Data(http.StatusOK, "text/html; charset=utf-8", assets.DefaultIndexPage)
		}
	})
}

// setDevProxyRouter 反向代理前端请求到 Rsbuild dev server，用于本地开发（HMR）。
// 支持 WebSocket 升级（HMR、lazy compilation），仅代理非 API 路径。
func setDevProxyRouter(router *gin.Engine, target string) {
	target = strings.TrimSuffix(target, "/")
	remote, err := url.Parse(target)
	if err != nil {
		common.FatalLog("invalid FRONTEND_DEV_PROXY URL: " + err.Error())
		return
	}
	common.SysLog("frontend dev proxy enabled -> " + target)

	proxy := httputil.NewSingleHostReverseProxy(remote)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = remote.Scheme
		req.URL.Host = remote.Host
		req.Host = remote.Host
	}

	router.NoRoute(func(c *gin.Context) {
		c.Set(middleware.RouteTagKey, "web")
		uri := c.Request.RequestURI
		// API 和 relay 路径不代理
		if strings.HasPrefix(uri, "/v1") || strings.HasPrefix(uri, "/api") {
			controller.RelayNotFound(c)
			return
		}
		// WebSocket 升级请求需要特殊处理
		if isWebSocketUpgrade(c.Request) {
			proxyWebSocket(c, remote)
			return
		}
		proxy.ServeHTTP(c.Writer, c.Request)
	})
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Connection"), "upgrade") &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func proxyWebSocket(c *gin.Context, remote *url.URL) {
	// 建立到上游的 TCP 连接
	targetAddr := remote.Host
	if !strings.Contains(targetAddr, ":") {
		if remote.Scheme == "wss" || remote.Scheme == "https" {
			targetAddr += ":443"
		} else {
			targetAddr += ":80"
		}
	}
	upstream, err := net.Dial("tcp", targetAddr)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream unreachable"})
		return
	}
	defer upstream.Close()

	// 把原始请求转发给上游
	if err := c.Request.Write(upstream); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to forward request"})
		return
	}

	// 接管客户端连接
	hijacker, ok := c.Writer.(http.Hijacker)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "hijack not supported"})
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "hijack failed"})
		return
	}
	defer client.Close()

	// 双向转发数据
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, client); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}
