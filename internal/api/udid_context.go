package api

import (
	"github.com/ms-robots/ms-robot/internal/device"

	"github.com/gin-gonic/gin"
)

type contextKey string

const (
	// ResolvedDeviceKey 单设备解析结果（/api/device/:udid 路由）
	ResolvedDeviceKey contextKey = "resolved_device"
	// ResolvedDevicesKey 多设备解析结果（/api/devices/:udids 路由）
	ResolvedDevicesKey contextKey = "resolved_devices"
)

// GetResolvedDevice 从 context 读取已解析的单设备（拦截器已解析并写入）
func GetResolvedDevice(c *gin.Context) (endpointID, deviceKey string, ok bool) {
	v, exists := c.Get(string(ResolvedDeviceKey))
	if !exists {
		return "", "", false
	}
	r, ok := v.(device.ResolvedDevice)
	if !ok {
		return "", "", false
	}
	return r.EndpointID, r.DeviceKey, true
}

// GetResolvedDevices 从 context 读取已解析的多设备
func GetResolvedDevices(c *gin.Context) ([]device.ResolvedDevice, bool) {
	v, exists := c.Get(string(ResolvedDevicesKey))
	if !exists {
		return nil, false
	}
	list, ok := v.([]device.ResolvedDevice)
	return list, ok
}

func apiUdidFromResolved(r device.ResolvedDevice) string {
	if r.EndpointID != "" {
		return r.DeviceKey + "@" + r.EndpointID
	}
	return r.DeviceKey
}
