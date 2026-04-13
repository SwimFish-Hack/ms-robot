package api

import (
	"github.com/ms-robots/ms-robot/internal/device"
	"net/http"

	"github.com/gin-gonic/gin"
)

// getActiveDeviceResolved 根据解析结果（endpointID + deviceKey）获取设备
func (s *Server) getActiveDeviceResolved(endpointID, deviceKey string) (*device.Device, bool) {
	return s.unifiedDeviceManager.GetDeviceInEndpoint(endpointID, deviceKey)
}

// deviceToMap 将设备转换为 map（共享辅助函数）。endpointID 为端点唯一 id，endpointDisplay 为显示名
func deviceToMap(device *device.Device, endpointID, endpointDisplay string) map[string]interface{} {
	m := map[string]interface{}{
		"udid":         device.UDID,
		"transport_id": device.TransportID,
		"status":       device.Status,
		"name":         device.Name,
		"model":        device.Model,
		"os_version":   device.OSVersion,
		"battery":      device.Battery,
		"resolution":   device.Resolution,
		"last_seen":    device.LastSeen,
		"connected_at": device.ConnectedAt,
		"endpoint":     endpointDisplay,
		"endpoint_id":  endpointID,
	}
	return m
}

// handleGetDevices 获取所有设备（按端点返回，含 endpoint_id / endpoint 显示名）
func (s *Server) handleGetDevices(c *gin.Context) {
	devicesByEndpoint := s.unifiedDeviceManager.GetAllDevicesWithEndpoint()
	var allDevices []map[string]interface{}
	for endpointAddr, devices := range devicesByEndpoint {
		endpointDisplay := s.endpointManager.GetEndpointDisplayName(endpointAddr)
		for _, d := range devices {
			allDevices = append(allDevices, deviceToMap(d, endpointAddr, endpointDisplay))
		}
	}
	c.JSON(http.StatusOK, gin.H{"devices": allDevices, "count": len(allDevices)})
}

// handleGetDevice 获取单个设备（udid 已由拦截器解析，从 context 取）。返回带 endpoint_id 便于前端用 apiUdid 定位卡片。
func (s *Server) handleGetDevice(c *gin.Context) {
	endpointID, deviceKey, ok := GetResolvedDevice(c)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少设备解析结果"})
		return
	}
	d, ok := s.unifiedDeviceManager.GetDeviceInEndpoint(endpointID, deviceKey)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备不存在"})
		return
	}
	endpointDisplay := s.endpointManager.GetEndpointDisplayName(endpointID)
	c.JSON(http.StatusOK, deviceToMap(d, endpointID, endpointDisplay))
}

// handleFileDevicesAdbPush 从 context 取解析后的设备列表并调用 fileHandler
func (s *Server) handleFileDevicesAdbPush(c *gin.Context) {
	list, ok := GetResolvedDevices(c)
	if !ok || len(list) == 0 {
		c.JSON(400, gin.H{"error": "缺少设备解析结果"})
		return
	}
	deviceKeys := make([]string, 0, len(list))
	for _, r := range list {
		deviceKeys = append(deviceKeys, r.DeviceKey)
	}
	s.fileHandler.HandleDevicesAdbPush(c, deviceKeys)
}

// handleFileDevicesAdbInstall 从 context 取解析后的设备列表并调用 fileHandler
func (s *Server) handleFileDevicesAdbInstall(c *gin.Context) {
	list, ok := GetResolvedDevices(c)
	if !ok || len(list) == 0 {
		c.JSON(400, gin.H{"error": "缺少设备解析结果"})
		return
	}
	deviceKeys := make([]string, 0, len(list))
	for _, r := range list {
		deviceKeys = append(deviceKeys, r.DeviceKey)
	}
	s.fileHandler.HandleDevicesAdbInstall(c, deviceKeys)
}
