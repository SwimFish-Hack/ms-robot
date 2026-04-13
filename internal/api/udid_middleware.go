package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/ms-robots/ms-robot/internal/device"

	"github.com/gin-gonic/gin"
)

// UDIDResolveMiddleware 对 /api/device/:udid 与 /api/devices/:udids 的请求解析 udid，写入 context；格式错误或设备未找到或多匹配时直接报错
func (s *Server) UDIDResolveMiddleware(c *gin.Context) {
	path := c.Request.URL.Path
	if !strings.HasPrefix(path, "/api/") {
		c.Next()
		return
	}
	path = strings.TrimPrefix(path, "/api/")
	parts := strings.SplitN(path, "/", 4)
	if len(parts) < 2 {
		c.Next()
		return
	}
	switch parts[0] {
	case "device":
		// 单设备：/api/device/:udid 或 /api/device/:udid/...
		udid := parts[1]
		if d, err := url.PathUnescape(udid); err == nil {
			udid = d
		}
		if udid == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "udid 不能为空"})
			c.Abort()
			return
		}
		resolved, err := s.resolveOne(udid)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			c.Abort()
			return
		}
		c.Set(string(ResolvedDeviceKey), resolved)
		c.Next()
		return
	case "devices":
		// 多设备：/api/devices/:udids 或 /api/devices/:udids/...
		udidsRaw := parts[1]
		if d, err := url.PathUnescape(udidsRaw); err == nil {
			udidsRaw = d
		}
		if udidsRaw == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "udids 不能为空"})
			c.Abort()
			return
		}
		udids := strings.Split(udidsRaw, ",")
		var list []device.ResolvedDevice
		for _, u := range udids {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			if decoded, err := url.PathUnescape(u); err == nil {
				u = decoded
			}
			resolved, err := s.resolveOne(u)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				c.Abort()
				return
			}
			list = append(list, resolved)
		}
		if len(list) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "udids 不能为空"})
			c.Abort()
			return
		}
		c.Set(string(ResolvedDevicesKey), list)
		c.Next()
		return
	}
	c.Next()
}

// resolveOne 解析单个 udid（单端点时也通过端点 id 解析）
func (s *Server) resolveOne(raw string) (device.ResolvedDevice, error) {
	return s.unifiedDeviceManager.ResolveDevice(raw)
}
