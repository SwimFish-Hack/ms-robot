package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"flag"
	"github.com/ms-robots/ms-robot/internal/api"
	"github.com/ms-robots/ms-robot/internal/config"
	"github.com/ms-robots/ms-robot/internal/device"
	"github.com/ms-robots/ms-robot/internal/endpoint"
	"github.com/ms-robots/ms-robot/internal/listenutil"
	"github.com/ms-robots/ms-robot/internal/logutil"
	"github.com/ms-robots/ms-robot/internal/streaming"
	"github.com/ms-robots/ms-robot/internal/websocket"
	"log"
	"strings"
	"time"
)

//go:embed assets/web assets/apks
var assetsFS embed.FS

// flagArray 支持多次指定同一参数
type flagArray []string

func (f *flagArray) String() string {
	return strings.Join(*f, ",")
}

func (f *flagArray) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func randomToken(prefix string, n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("❌ 生成随机凭证失败: %v", err)
	}
	return prefix + hex.EncodeToString(buf)
}

func main() {
	// 解析命令行参数（listen URL 支持 tcp://:端口、unix:///path 等）
	httpListen := flag.String("http-listen", "tcp://:20605", "HTTP 监听地址 (默认: tcp://:20605)")
	turnPort := flag.Int("turn-port", 3478, "TURN服务器监听端口 (默认: 3478)")
	debugMode := flag.Bool("debug", false, "开启调试模式（允许前端在 ?debug=1 时加载 vConsole 等，并启用更多调试开关）")
	withoutEndpoint := flag.Bool("without-endpoint", false, "不添加默认端点；未指定任何 -endpoint 时端点列表为空")
	endpointsMutable := flag.Bool("endpoints-mutable", false, "允许通过界面或接口编辑端点（添加/删除/修改）；关闭时相关接口返回未开放此功能")
	var endpoints flagArray
	flag.Var(&endpoints, "endpoint", `ADB 端点，可多次指定。
  示例:
  adb=192.168.1.100
  adb=192.168.1.100,name=本机,retry=-1
  adb=192.168.1.100:5037,name=本机,proxy=socks5://user:pass@proxy:1080,retry=-1
  retry=-1,name=本机,adb=localhost
  未指定任何 -endpoint 时默认 adb=localhost,name=本机（retry 不填默认 -1 一直重试）。`)
	flag.Parse()

	// 未指定端点且未设 without-endpoint 时，使用默认单端点
	if len(endpoints) == 0 && !*withoutEndpoint {
		endpoints = append(endpoints, "adb=localhost,name=本机")
	}

	// 初始化端点管理器（稍后初始化完成再添加端点）
	endpointManager := endpoint.NewManager()

	// 统一设备管理器（先不添加端点）
	unifiedDeviceManager := device.NewUnifiedDeviceManager(endpointManager)

	wsHub := websocket.NewHub()
	go wsHub.Run()

	// 重连相关回调：更新状态并广播给前端（该端点 retry!=0 时生效）
	unifiedDeviceManager.SetOnReconnecting(func(endpointAddr string) {
		unifiedDeviceManager.SetEndpointStatus(endpointAddr, device.EndpointStatusReconnecting)
		displayName := endpointManager.GetEndpointDisplayName(endpointAddr)
		wsHub.BroadcastEndpointStatus(endpointAddr, displayName, device.EndpointStatusReconnecting)
	})
	unifiedDeviceManager.SetOnReconnected(func(endpointAddr string) {
		unifiedDeviceManager.SetEndpointStatus(endpointAddr, device.EndpointStatusOK)
		displayName := endpointManager.GetEndpointDisplayName(endpointAddr)
		wsHub.BroadcastEndpointStatus(endpointAddr, displayName, device.EndpointStatusOK)
	})
	unifiedDeviceManager.SetOnReconnectFailed(func(endpointAddr string) {
		unifiedDeviceManager.SetEndpointStatus(endpointAddr, device.EndpointStatusReconnectFailed)
		displayName := endpointManager.GetEndpointDisplayName(endpointAddr)
		wsHub.BroadcastEndpointStatus(endpointAddr, displayName, device.EndpointStatusReconnectFailed)
	})

	// 断线回调：该端点 retry=0 时自动移除并广播「已移除」；retry!=0 时仅由上面回调更新状态
	unifiedDeviceManager.SetOnEndpointConnectionLost(func(endpointAddr string) {
		if endpointManager.GetRetry(endpointAddr) != 0 {
			return
		}
		displayName := endpointManager.GetEndpointDisplayName(endpointAddr)
		unifiedDeviceManager.SetEndpointStatus(endpointAddr, device.EndpointStatusRemoved)
		wsHub.BroadcastEndpointStatus(endpointAddr, displayName, device.EndpointStatusRemoved)
		if mgr, ok := unifiedDeviceManager.GetManagerForEndpoint(endpointAddr); ok {
			for _, d := range mgr.GetAllDevices() {
				d.Status = device.StatusOffline
				wsHub.BroadcastDeviceStatus(d)
			}
		}
		removedKey, err := endpointManager.RemoveEndpoint(endpointAddr)
		if err != nil {
			logutil.Warnf("[main] 端点 %s 连接已断开，自动移除失败: %v", endpointAddr, err)
			return
		}
		unifiedDeviceManager.OnEndpointRemoved(removedKey)
		logutil.Infof("[main] 端点 %s 连接已断开，已自动移除", endpointAddr)
	})

	if err := unifiedDeviceManager.Start(); err != nil {
		log.Fatalf("❌ 启动统一设备管理器失败: %v", err)
	}

	// 初始化WebRTC管理器与常驻 scrcpy 池（关投屏只停读流，scrcpy 不退出）
	webrtcManager := streaming.GetWebRTCManager()
	webrtcManager.SetUnifiedDeviceManager(unifiedDeviceManager)
	webrtcManager.SetAssetsFS(assetsFS)
	scrcpyPool := streaming.NewDeviceScrcpyPool()
	webrtcManager.SetScrcpyPool(scrcpyPool)

	// 启动内置TURN服务器
	turnRealm := randomToken("turn-realm-", 8)
	turnUsername := randomToken("turn-user-", 8)
	turnPassword := randomToken("turn-pass-", 12)

	turnServer, err := streaming.NewTurnServer(*turnPort, turnRealm, turnUsername, turnPassword)
	if err != nil {
		log.Fatalf("❌ 启动内置TURN服务器失败: %v (端口 %d 可能已被占用，请使用 -turn-port 参数指定其他端口)", err, *turnPort)
	}
	if err := turnServer.Start(); err != nil {
		log.Fatalf("❌ 启动内置TURN服务器失败: %v (端口 %d 可能已被占用，请使用 -turn-port 参数指定其他端口)", err, *turnPort)
	}
	webrtcManager.SetLocalTurnServer(turnServer)
	logutil.Infof("✅ 内置TURN服务器已启动，监听端口: %d", *turnPort)
	logutil.Infof("📌 STUN/TURN地址将在客户端请求时根据其Host头动态生成 (stun:{host}:%d)", *turnPort)

	// 不需要设置外部ICE服务器（使用内置TURN）
	webrtcManager.SetICEServers([]string{}, []string{})

	server := api.NewServer(
		unifiedDeviceManager,
		endpointManager,
		wsHub,
		assetsFS,
		*endpointsMutable,
		*debugMode,
		config.App,
	)

	// 启动定时状态更新
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			devices := unifiedDeviceManager.GetAllDevices()
			for _, d := range devices {
				wsHub.BroadcastDeviceStatus(d)
			}
		}
	}()

	ctx := context.Background()
	listener, err := listenutil.Listen(ctx, *httpListen)
	if err != nil {
		log.Fatalf("❌ HTTP 监听失败: %v", err)
	}
	defer listener.Close()

	logutil.Infof("🚀 设备控制平台启动中...")
	logutil.Infof("📱 HTTP 监听 %s", *httpListen)

	// 先完成所有不依赖端点的初始化，再连接端点

	for _, ep := range endpoints {
		var id string
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			id, err = endpointManager.AddEndpoint(ep, true)
			if err == nil {
				break
			}
			if attempt < 2 {
				logutil.Warnf("[main] 添加端点 %s 失败，2 秒后重试: %v", ep, err)
				time.Sleep(2 * time.Second)
			}
		}
		if err != nil {
			logutil.Errorf("[main] 添加端点失败，已跳过 %s: %v", ep, err)
			continue
		}
		unifiedDeviceManager.OnEndpointAdded(id)
	}
	eps := endpointManager.GetEndpoints()
	for _, ep := range eps {
		if mgr, ok := unifiedDeviceManager.GetManagerForEndpoint(ep); ok {
			mgr.SetWSHub(wsHub)
			epID := ep
			mgr.SetOnDeviceDisconnect(func(udid string) {
				webrtcKey := udid
				if epID != "" {
					webrtcKey = udid + "@" + epID
				}
				webrtcManager.HandleManualDisconnect(webrtcKey)
			})
		}
	}

	logutil.Infof("📡 已添加端点：%d 个", len(eps))
	for _, id := range eps {
		logutil.Infof("  - %s", id)
	}
	logutil.Infof("🔗 内置TURN服务器: 端口 %d (realm: %s, 用户名: %s, 密码: [hidden])",
		*turnPort, turnRealm, turnUsername)

	if err := server.Serve(ctx, listener); err != nil {
		log.Fatal("服务器启动失败:", err)
	}
}
