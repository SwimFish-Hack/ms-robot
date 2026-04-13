package api

import (
	"fmt"
	"github.com/ms-robots/ms-robot/internal/logutil"
	"github.com/ms-robots/ms-robot/internal/streaming"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// handleWebRTCOffer 处理WebRTC Offer（使用中间件解析的 endpointID+deviceKey 查设备）
func (s *Server) handleWebRTCOffer(c *gin.Context) {
	endpointID, deviceKey, ok := GetResolvedDevice(c)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少设备解析结果"})
		return
	}
	webrtcManager := streaming.GetWebRTCManager()
	webrtcManager.HandleWebRTCOfferResolved(c, endpointID, deviceKey)
}

// handleWebRTCICE 处理WebRTC ICE候选（与 offer 使用相同的 deviceUDID 规则，避免多端点时 key 不一致）
func (s *Server) handleWebRTCICE(c *gin.Context) {
	endpointID, deviceKey, ok := GetResolvedDevice(c)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少设备解析结果"})
		return
	}
	deviceUDID := deviceKey
	if endpointID != "" {
		deviceUDID = deviceKey + "@" + endpointID
	}
	webrtcManager := streaming.GetWebRTCManager()
	webrtcManager.HandleICE(c, deviceUDID)
}

// handleWebRTCDisconnect 处理WebRTC断开连接（手动断开）
func (s *Server) handleWebRTCDisconnect(c *gin.Context) {
	endpointID, deviceKey, ok := GetResolvedDevice(c)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少设备解析结果"})
		return
	}
	deviceUDID := deviceKey
	if endpointID != "" {
		deviceUDID = deviceKey + "@" + endpointID
	}
	var body struct {
		SessionID string `json:"sessionId"`
	}
	// 无 body 的旧客户端：整设备断开；有 sessionId 时只摘该路 WebRTC，避免多开页面互相踢掉
	_ = c.ShouldBindJSON(&body)
	sessionID := strings.TrimSpace(body.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(c.Query("sessionId"))
	}
	if sessionID != "" {
		logutil.Debugf("[API] 收到设备 %s 的手动断开单会话请求 sessionId=%s", deviceUDID, sessionID)
	} else {
		logutil.Debugf("[API] 收到设备 %s 的手动断开连接请求（整设备）", deviceUDID)
	}

	webrtcManager := streaming.GetWebRTCManager()
	webrtcManager.DisconnectWebRTC(deviceUDID, sessionID)

	c.JSON(http.StatusOK, gin.H{"message": "断开连接请求已处理"})
}

// handleWebRTCStatus 获取WebRTC连接状态
func (s *Server) handleWebRTCStatus(c *gin.Context) {
	endpointID, deviceKey, ok := GetResolvedDevice(c)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少设备解析结果"})
		return
	}
	deviceUDID := deviceKey
	if endpointID != "" {
		deviceUDID = deviceKey + "@" + endpointID
	}

	webrtcManager := streaming.GetWebRTCManager()
	sessions, exists := webrtcManager.GetStreamSessions(deviceUDID)
	if !exists || len(sessions) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"connected": false,
			"sessions":  []interface{}{},
			"count":     0,
		})
		return
	}

	// 构建会话状态信息
	sessionStatuses := make([]map[string]interface{}, 0, len(sessions))
	for sessionID, session := range sessions {
		status := map[string]interface{}{
			"session_id": sessionID,
		}

		if session.PeerConn != nil {
			status["connection_state"] = session.PeerConn.ConnectionState().String()
			status["ice_connection_state"] = session.PeerConn.ICEConnectionState().String()
			status["signaling_state"] = session.PeerConn.SignalingState().String()
		}

		if session.DataChannel != nil {
			status["data_channel_state"] = session.DataChannel.ReadyState().String()
		}

		sessionStatuses = append(sessionStatuses, status)
	}

	c.JSON(http.StatusOK, gin.H{
		"connected": true,
		"sessions":  sessionStatuses,
		"count":     len(sessions),
	})
}

// handleGetICEServers 获取ICE服务器配置（供前端使用）
func (s *Server) handleGetICEServers(c *gin.Context) {
	webrtcManager := streaming.GetWebRTCManager()
	stunServers, turnServers := webrtcManager.GetICEServers()

	// 获取请求的host（用于构建STUN/TURN服务器地址）
	host := c.Request.Host
	if host == "" {
		host = "127.0.0.1"
	} else {
		// 移除端口号，只保留host（因为STUN/TURN使用自己的端口）
		if idx := strings.Index(host, ":"); idx > 0 {
			host = host[:idx]
		}
	}

	// 默认使用内置TURN服务器的端口作为STUN（端口3478）
	localTurnServer := webrtcManager.GetLocalTurnServer()
	if localTurnServer != nil {
		turnPort := localTurnServer.GetPort()
		// 默认使用 stun:{host}:3478
		stunServers = []string{fmt.Sprintf("stun:%s:%d", host, turnPort)}
		logutil.Debugf("[API] 为客户端 %s (Host: %s) 返回STUN地址: stun:%s:%d", c.Request.RemoteAddr, host, host, turnPort)
	} else {
		// 如果没有内置TURN服务器，使用默认STUN（不应该发生，因为默认启动TURN）
		if len(stunServers) == 0 {
			stunServers = []string{fmt.Sprintf("stun:%s:3478", host)}
			logutil.Debugf("[API] 警告: 内置TURN服务器未启动，使用默认STUN: stun:%s:3478", host)
		}
	}

	// 构建TURN服务器列表（如果启动了内置TURN，添加内置TURN地址）
	finalTurnServers := turnServers
	if localTurnServer != nil {
		turnPort := localTurnServer.GetPort()
		turnURL := fmt.Sprintf("turn:%s:%d?username=%s&credential=%s",
			host, turnPort, localTurnServer.GetUsername(), localTurnServer.GetPassword())
		// 检查是否已存在（避免重复）
		exists := false
		for _, ts := range turnServers {
			if strings.Contains(ts, fmt.Sprintf(":%d", turnPort)) {
				exists = true
				break
			}
		}
		if !exists {
			finalTurnServers = append(finalTurnServers, turnURL)
			logutil.Debugf("[API] 为客户端 %s (Host: %s) 添加内置TURN地址: %s", c.Request.RemoteAddr, host, turnURL)
		}
	}

	logutil.Debugf("[API] 客户端 %s 请求ICE服务器配置: STUN=%v, TURN=%v", c.Request.RemoteAddr, stunServers, finalTurnServers)

	c.JSON(http.StatusOK, gin.H{
		"stun": stunServers,
		"turn": finalTurnServers, // 使用包含内置TURN服务器的列表
		"host": host,             // 返回当前服务器的host，前端可以用它构建STUN/TURN地址
	})
}

// handleScrcpyAudioEnabled 设置是否采集并转发设备音频（需先有投屏连接）
func (s *Server) handleScrcpyAudioEnabled(c *gin.Context) {
	endpointID, deviceKey, ok := GetResolvedDevice(c)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少设备解析结果"})
		return
	}
	deviceUDID := deviceKey
	if endpointID != "" {
		deviceUDID = deviceKey + "@" + endpointID
	}

	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "需要 body: {\"enabled\": true|false}"})
		return
	}

	webrtcManager := streaming.GetWebRTCManager()
	session, exists := webrtcManager.GetStream(deviceUDID)
	var streamer *streaming.AndroidStreamer
	if exists && session != nil {
		streamer = session.GetStreamer()
	}
	if streamer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "请先建立投屏连接"})
		return
	}
	scrcpyServer := streamer.GetScrcpyServer()
	if scrcpyServer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "scrcpy 未就绪"})
		return
	}

	if err := scrcpyServer.SetAudioEnabled(*req.Enabled); err != nil {
		logutil.Warnf("[API] 设备 %s 设置音频启用失败: %v", deviceUDID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if *req.Enabled {
		scrcpyServer.SetAudioSink(streamer)
	}
	logutil.Debugf("[API] 设备 %s 音频采集 enabled=%v", deviceUDID, *req.Enabled)

	c.JSON(http.StatusOK, gin.H{"enabled": *req.Enabled})
}
