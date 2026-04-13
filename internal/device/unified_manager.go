package device

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/ms-robots/ms-robot/internal/endpoint"
	"github.com/ms-robots/ms-robot/internal/logutil"
	adb "github.com/ms-robots/ms-robot/third_party/adb"
)

// 端点连接状态（用于前端展示）
const (
	EndpointStatusOK              = "ok"
	EndpointStatusReconnecting    = "reconnecting"
	EndpointStatusReconnectFailed = "reconnect_failed"
	EndpointStatusRemoved         = "removed"
)

// UnifiedDeviceManager 统一设备管理器（管理多个端点的设备）
type UnifiedDeviceManager struct {
	endpointManager          *endpoint.Manager
	managers                 sync.Map // endpoint (string) -> *Manager
	endpointStatus           sync.Map // endpoint (string) -> status (string)
	onEndpointConnectionLost func(endpointAddr string)
	onReconnecting           func(endpointAddr string)
	onReconnected            func(endpointAddr string)
	onReconnectFailed        func(endpointAddr string)
	callbacksMu              sync.RWMutex
}

// NewUnifiedDeviceManager 创建统一设备管理器
func NewUnifiedDeviceManager(endpointManager *endpoint.Manager) *UnifiedDeviceManager {
	return &UnifiedDeviceManager{endpointManager: endpointManager}
}

// Start 启动所有端点的设备发现
func (u *UnifiedDeviceManager) Start() error {
	endpoints := u.endpointManager.GetEndpoints()

	for _, ep := range endpoints {
		if err := u.addEndpointManager(ep); err != nil {
			logutil.Warnf("[UnifiedDeviceManager] 添加端点管理器失败 %s: %v", ep, err)
			continue
		}
	}

	return nil
}

// GetEndpointStatus 获取端点连接状态
func (u *UnifiedDeviceManager) GetEndpointStatus(endpointAddr string) string {
	if v, ok := u.endpointStatus.Load(endpointAddr); ok {
		return v.(string)
	}
	return EndpointStatusOK
}

// SetEndpointStatus 设置端点连接状态（供 main 回调里更新并广播）
func (u *UnifiedDeviceManager) SetEndpointStatus(endpointAddr, status string) {
	u.endpointStatus.Store(endpointAddr, status)
}

// SetOnEndpointConnectionLost 设置某端点 adb 连接断开时的回调（如 watcher 因错误退出）
func (u *UnifiedDeviceManager) SetOnEndpointConnectionLost(callback func(endpointAddr string)) {
	u.callbacksMu.Lock()
	defer u.callbacksMu.Unlock()
	u.onEndpointConnectionLost = callback
}

// SetOnReconnecting 设置开始重连时的回调
func (u *UnifiedDeviceManager) SetOnReconnecting(callback func(endpointAddr string)) {
	u.callbacksMu.Lock()
	defer u.callbacksMu.Unlock()
	u.onReconnecting = callback
}

// SetOnReconnected 设置重连成功时的回调
func (u *UnifiedDeviceManager) SetOnReconnected(callback func(endpointAddr string)) {
	u.callbacksMu.Lock()
	defer u.callbacksMu.Unlock()
	u.onReconnected = callback
}

// SetOnReconnectFailed 设置重连失败时的回调
func (u *UnifiedDeviceManager) SetOnReconnectFailed(callback func(endpointAddr string)) {
	u.callbacksMu.Lock()
	defer u.callbacksMu.Unlock()
	u.onReconnectFailed = callback
}

func (u *UnifiedDeviceManager) invokeOnEndpointConnectionLost(endpointAddr string) {
	u.callbacksMu.RLock()
	cb := u.onEndpointConnectionLost
	u.callbacksMu.RUnlock()
	if cb != nil {
		cb(endpointAddr)
	}
}

func (u *UnifiedDeviceManager) invokeOnReconnecting(endpointAddr string) {
	u.callbacksMu.RLock()
	cb := u.onReconnecting
	u.callbacksMu.RUnlock()
	if cb != nil {
		cb(endpointAddr)
	}
}

func (u *UnifiedDeviceManager) invokeOnReconnected(endpointAddr string) {
	u.callbacksMu.RLock()
	cb := u.onReconnected
	u.callbacksMu.RUnlock()
	if cb != nil {
		cb(endpointAddr)
	}
}

func (u *UnifiedDeviceManager) invokeOnReconnectFailed(endpointAddr string) {
	u.callbacksMu.RLock()
	cb := u.onReconnectFailed
	u.callbacksMu.RUnlock()
	if cb != nil {
		cb(endpointAddr)
	}
}

// addEndpointManager 为端点创建设备管理器
func (u *UnifiedDeviceManager) addEndpointManager(endpointAddr string) error {
	adbClient, ok := u.endpointManager.GetClient(endpointAddr)
	if !ok {
		logutil.Warnf("[UnifiedDeviceManager] 端点 %s ADB 客户端不存在", endpointAddr)
		return fmt.Errorf("端点 ADB 客户端不存在: %s", endpointAddr)
	}

	host, _ := u.endpointManager.GetHost(endpointAddr)
	port, _ := u.endpointManager.GetPort(endpointAddr)
	logutil.Debugf("[UnifiedDeviceManager] 端点 %s 创建设备管理器 adb=%s:%d", endpointAddr, host, port)
	var dialTCP DialTCPFunc
	if ed := u.endpointManager.GetDialTCP(endpointAddr); ed != nil {
		dialTCP = func(addr string) (net.Conn, error) { return ed(addr) }
	}
	deviceMgr := NewManagerWithAdbClient(adbClient, host, port, dialTCP)
	if deviceMgr == nil {
		return fmt.Errorf("创建设备管理器失败")
	}

	retryN := u.endpointManager.GetRetry(endpointAddr)
	deviceMgr.SetRetryCount(retryN)

	addr := endpointAddr
	deviceMgr.SetOnConnectionLost(func() { u.invokeOnEndpointConnectionLost(addr) })
	deviceMgr.SetOnReconnecting(func() { u.invokeOnReconnecting(addr) })
	deviceMgr.SetOnReconnected(func() { u.invokeOnReconnected(addr) })
	deviceMgr.SetOnReconnectFailed(func() { u.invokeOnReconnectFailed(addr) })

	u.managers.Store(endpointAddr, deviceMgr)
	u.endpointStatus.Store(endpointAddr, EndpointStatusOK)

	// 先同步执行一次设备发现，再启动后台监控，这样添加端点后前端立即刷新即可看到设备
	deviceMgr.RunInitialDiscovery()
	go deviceMgr.StartWatcher()
	logutil.Infof("[UnifiedDeviceManager] 端点 %s 已启动设备发现", endpointAddr)
	return nil
}

// removeEndpointManager 移除端点的设备管理器
func (u *UnifiedDeviceManager) removeEndpointManager(endpointAddr string) {
	if v, ok := u.managers.Load(endpointAddr); ok {
		if mgr, _ := v.(*Manager); mgr != nil {
			mgr.Stop()
		}
		u.managers.Delete(endpointAddr)
		u.endpointStatus.Delete(endpointAddr)
	}
}

// GetAllDevices 获取所有端点的设备
func (u *UnifiedDeviceManager) GetAllDevices() []*Device {
	var allDevices []*Device
	u.managers.Range(func(_, value interface{}) bool {
		if mgr, ok := value.(*Manager); ok && mgr != nil {
			allDevices = append(allDevices, mgr.GetAllDevices()...)
		}
		return true
	})
	return allDevices
}

// GetAllDevicesWithEndpoint 获取所有设备及其所属端点
func (u *UnifiedDeviceManager) GetAllDevicesWithEndpoint() map[string][]*Device {
	result := make(map[string][]*Device)
	u.managers.Range(func(key, value interface{}) bool {
		if endpointAddr, ok := key.(string); ok {
			if mgr, ok := value.(*Manager); ok && mgr != nil {
				result[endpointAddr] = mgr.GetAllDevices()
			}
		}
		return true
	})
	return result
}

// GetDevice 获取设备（在所有端点中查找）
func (u *UnifiedDeviceManager) GetDevice(udid string) (*Device, bool) {
	var found *Device
	u.managers.Range(func(_, value interface{}) bool {
		if mgr, ok := value.(*Manager); ok && mgr != nil {
			if device, ok := mgr.GetDevice(udid); ok {
				found = device
				return false
			}
		}
		return true
	})
	return found, found != nil
}

// GetADBDevice 获取 ADB Device（在所有端点中查找）。key 为 serial 或 serial:transportID
func (u *UnifiedDeviceManager) GetADBDevice(key string) (*adb.Device, string, error) {
	var foundDevice *adb.Device
	var foundEp string
	u.managers.Range(func(k, value interface{}) bool {
		if endpointAddr, ok := k.(string); ok {
			if mgr, ok := value.(*Manager); ok && mgr != nil {
				if adbDevice, err := mgr.GetADBDevice(key); err == nil {
					foundDevice = adbDevice
					foundEp = endpointAddr
					return false
				}
			}
		}
		return true
	})
	if foundDevice != nil {
		return foundDevice, foundEp, nil
	}
	return nil, "", fmt.Errorf("设备未找到: %s", key)
}

// GetDeviceInEndpoint 在指定端点按 key 获取设备
func (u *UnifiedDeviceManager) GetDeviceInEndpoint(endpointID, key string) (*Device, bool) {
	mgr, ok := u.GetManagerForEndpoint(endpointID)
	if !ok {
		return nil, false
	}
	return mgr.GetDevice(key)
}

// GetADBDeviceInEndpoint 在指定端点按 key 获取 ADB Device
func (u *UnifiedDeviceManager) GetADBDeviceInEndpoint(endpointID, key string) (*adb.Device, error) {
	mgr, ok := u.GetManagerForEndpoint(endpointID)
	if !ok {
		return nil, fmt.Errorf("端点不存在: %s", endpointID)
	}
	return mgr.GetADBDevice(key)
}

// GetManagerForEndpoint 获取端点的设备管理器
func (u *UnifiedDeviceManager) GetManagerForEndpoint(endpointAddr string) (*Manager, bool) {
	v, ok := u.managers.Load(endpointAddr)
	if !ok {
		return nil, false
	}
	mgr, _ := v.(*Manager)
	return mgr, mgr != nil
}

// OnEndpointAdded 端点添加时的回调
func (u *UnifiedDeviceManager) OnEndpointAdded(endpointAddr string) {
	u.addEndpointManager(endpointAddr)
}

// OnEndpointRemoved 端点删除时的回调
func (u *UnifiedDeviceManager) OnEndpointRemoved(endpointAddr string) {
	u.removeEndpointManager(endpointAddr)
}

// ResolvedDevice 解析后的设备标识（供 API 使用）
type ResolvedDevice struct {
	EndpointID string // 端点 id
	DeviceKey  string // serial 或 serial:transportID
}

// ParseUDID 解析请求中的 udid 字符串。格式：udid[:transport_id][@endpoint_id]，返回 serial、transportID、endpointID（均为可选未设时为空/0）
func ParseUDID(raw string) (serial string, transportID int, endpointID string, err error) {
	raw = trimSpace(raw)
	if raw == "" {
		return "", 0, "", fmt.Errorf("udid 不能为空")
	}
	// 可选 @endpoint_id
	if idx := strings.LastIndex(raw, "@"); idx >= 0 {
		endpointID = trimSpace(raw[idx+1:])
		raw = trimSpace(raw[:idx])
		if raw == "" {
			return "", 0, "", fmt.Errorf("udid 格式错误: @ 前缺少 serial")
		}
	}
	// 可选 :transport_id
	if idx := strings.LastIndex(raw, ":"); idx > 0 && idx < len(raw)-1 {
		if n, e := strconv.Atoi(trimSpace(raw[idx+1:])); e == nil && n >= 0 {
			transportID = n
			raw = trimSpace(raw[:idx])
		}
	}
	if raw == "" {
		return "", 0, "", fmt.Errorf("udid 格式错误: 缺少 serial")
	}
	serial = raw
	return serial, transportID, endpointID, nil
}

func trimSpace(s string) string { return strings.TrimSpace(s) }

// ResolveDevice 将请求中的 udid 解析为唯一设备。若未指定 endpoint_id/transport_id 且匹配多台则返回错误
func (u *UnifiedDeviceManager) ResolveDevice(raw string) (ResolvedDevice, error) {
	serial, transportID, endpointID, err := ParseUDID(raw)
	if err != nil {
		return ResolvedDevice{}, err
	}
	key := DeviceKey(serial, transportID)

	var matches []ResolvedDevice
	u.managers.Range(func(keyInterface, value interface{}) bool {
		epID, ok := keyInterface.(string)
		if !ok {
			return true
		}
		if endpointID != "" && epID != endpointID {
			return true
		}
		mgr, _ := value.(*Manager)
		if mgr == nil {
			return true
		}
		if transportID > 0 {
			if _, ok := mgr.GetDevice(key); ok {
				matches = append(matches, ResolvedDevice{EndpointID: epID, DeviceKey: key})
			}
		} else {
			for _, d := range mgr.GetAllDevices() {
				if d.UDID != serial {
					continue
				}
				k := DeviceKey(d.UDID, d.TransportID)
				matches = append(matches, ResolvedDevice{EndpointID: epID, DeviceKey: k})
			}
		}
		return true
	})

	if len(matches) == 0 {
		return ResolvedDevice{}, fmt.Errorf("设备未找到: %s", raw)
	}
	if len(matches) > 1 {
		return ResolvedDevice{}, fmt.Errorf("匹配到多台设备，请精确指明 endpoint_id 或 transport_id（当前 udid: %s）", raw)
	}
	return matches[0], nil
}
