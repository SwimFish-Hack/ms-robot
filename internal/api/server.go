package api

import (
	"context"
	"embed"
	"github.com/ms-robots/ms-robot/internal/device"
	"github.com/ms-robots/ms-robot/internal/endpoint"
	"github.com/ms-robots/ms-robot/internal/files"
	"github.com/ms-robots/ms-robot/internal/listenutil"
	"github.com/ms-robots/ms-robot/internal/logutil"
	ws "github.com/ms-robots/ms-robot/internal/websocket"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Server HTTP API服务器（仅使用 UnifiedDeviceManager，端点由 endpointManager 管理）
type Server struct {
	unifiedDeviceManager *device.UnifiedDeviceManager
	endpointManager      *endpoint.Manager
	wsHub                *ws.Hub
	router               *gin.Engine
	fileHandler          *files.Handler
	webFS                embed.FS
	endpointsMutable     bool // 端点是否可变（允许通过界面/接口添加/删除/修改）
	debugMode            bool // 全局调试开关（由 --debug 控制）
	appConfig            AppConfig
}

type AppConfig struct {
	AppName string
}

// NewServer 创建 API 服务器。endpointsMutable 为 false 时端点只读，禁止添加/删除/修改。debugMode 为全局调试开关（由 --debug 控制）。
func NewServer(unifiedDeviceManager *device.UnifiedDeviceManager, endpointManager *endpoint.Manager, wsHub *ws.Hub, webFS embed.FS, endpointsMutable bool, debugMode bool, appConfig AppConfig) *Server {
	fileManager, err := files.NewManager()
	if err != nil {
		logutil.Warnf("警告: 初始化文件管理器失败: %v", err)
		fileManager = nil
	}
	var fileHandler *files.Handler
	if fileManager != nil {
		fileHandler = files.NewHandler(fileManager, unifiedDeviceManager)
	}
	server := &Server{
		unifiedDeviceManager: unifiedDeviceManager,
		endpointManager:      endpointManager,
		wsHub:                wsHub,
		router:               gin.Default(),
		fileHandler:          fileHandler,
		webFS:                webFS,
		endpointsMutable:     endpointsMutable,
		debugMode:            debugMode,
		appConfig:            appConfig,
	}

	server.setupRoutes()
	return server
}

// setupRoutes 设置路由
func (s *Server) setupRoutes() {
	// 从 embed.FS 加载模板
	tmpl, err := template.ParseFS(s.webFS, "assets/web/templates/*")
	if err != nil {
		log.Fatalf("加载模板失败: %v", err)
	}
	s.router.SetHTMLTemplate(tmpl)

	// 静态文件服务（从 embed.FS）
	staticFS, err := fs.Sub(s.webFS, "assets/web/static")
	if err != nil {
		log.Fatalf("加载静态文件失败: %v", err)
	}
	s.router.StaticFS("/static", http.FS(staticFS))

	// API路由组；对 device/devices 的 udid/udids 做解析并写入 context
	// 约定：/api/device/:udid/* 单设备；/api/devices/:udids/* 可批量（udids 逗号分隔）
	api := s.router.Group("/api")
	api.Use(s.UDIDResolveMiddleware)
	{
		// 设备管理
		api.GET("/devices", s.handleGetDevices)
		api.GET("/device/:udid", s.handleGetDevice)

		// WebSocket
		api.GET("/ws", s.handleWebSocket)

		// WebRTC流（单设备）
		api.POST("/device/:udid/webrtc/offer", s.handleWebRTCOffer)
		api.POST("/device/:udid/webrtc/ice", s.handleWebRTCICE)
		api.POST("/device/:udid/webrtc/disconnect", s.handleWebRTCDisconnect)
		api.GET("/device/:udid/webrtc/status", s.handleWebRTCStatus)
		api.GET("/webrtc/ice-servers", s.handleGetICEServers) // 获取ICE服务器配置

		// 终端能力：仅保留单设备交互式 shell
		api.GET("/device/:udid/adb/shell", s.handleShellWebSocket)

		// scrcpy 音频：启用/停用设备音频采集并转发（需先有投屏连接）
		api.PUT("/device/:udid/scrcpy/audio/enabled", s.handleScrcpyAudioEnabled)

		// 文件管理
		// DELETE /api/files：按时间条件清理（query: upload_time_at 或 upload_time_before，Unix 秒）
		// DELETE /api/files/:ids：按 id 批量删除（:ids 逗号分隔）
		if s.fileHandler != nil {
			api.POST("/files/check", s.fileHandler.HandleCheckMD5)                        // 检查文件MD5是否存在
			api.POST("/files/upload", s.fileHandler.HandleUpload)                         // 上传文件
			api.GET("/files/list", s.fileHandler.HandleListFiles)                         // 获取文件列表
			api.DELETE("/files", s.fileHandler.HandleCleanupFiles)                        // 条件删除
			api.DELETE("/files/:ids", s.fileHandler.HandleDeleteFiles)                    // 按 id 批量删除
			api.POST("/devices/:udids/adb/push/:files", s.handleFileDevicesAdbPush)       // 多设备多文件推送，body 为路径配置
			api.POST("/devices/:udids/adb/install/:files", s.handleFileDevicesAdbInstall) // 多设备多文件安装 APK
		}

		// 端点管理
		api.GET("/endpoints", s.HandleGetEndpoints)
		api.POST("/endpoints", s.HandleAddEndpoint)
		api.DELETE("/endpoints/:endpoint", s.HandleRemoveEndpoint)
	}

	// 未匹配的路由：/ 或空走首页 index.html，其余尝试按路径渲染模板
	s.router.NoRoute(func(c *gin.Context) {
		path := strings.TrimPrefix(c.Request.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if strings.HasPrefix(path, "api/") {
			c.JSON(http.StatusNotFound, gin.H{"error": "接口不存在"})
			return
		}
		if s.router.HTMLRender != nil {
			c.HTML(http.StatusOK, path, s.templateData())
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "页面未找到"})
	})
}

func (s *Server) templateData() gin.H {
	return gin.H{
		"DebugMode": s.debugMode,
		"AppName":   s.appConfig.AppName,
	}
}

// Start 启动服务器，listenURL 支持 tcp://:20605、unix:///path/to.sock 等
func (s *Server) Start(ctx context.Context, listenURL string) error {
	listener, err := listenutil.Listen(ctx, listenURL)
	if err != nil {
		return err
	}
	defer listener.Close()
	return s.Serve(ctx, listener)
}

// Serve 使用已有 listener 启动 HTTP 服务（便于先 Listen 再添加端点、再 Serve）
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	srv := &http.Server{Handler: s.router}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return srv.Serve(listener)
}
