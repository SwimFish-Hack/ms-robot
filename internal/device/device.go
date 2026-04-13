package device

import (
	"github.com/ms-robots/ms-robot/internal/logutil"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	adb "github.com/ms-robots/ms-robot/third_party/adb"
)

// DeviceKey 返回 Manager 内存储的 key：transportID>0 时为 "serial:transportID"，否则为 serial
func DeviceKey(serial string, transportID int) string {
	if transportID > 0 {
		return serial + ":" + strconv.Itoa(transportID)
	}
	return serial
}

// parseDeviceKey 解析 key 为 serial 与可选的 transportID。格式 serial 或 serial:transportID
func parseDeviceKey(key string) (serial string, transportID int) {
	idx := strings.LastIndex(key, ":")
	if idx <= 0 || idx == len(key)-1 {
		return key, 0
	}
	if n, err := strconv.Atoi(key[idx+1:]); err == nil && n > 0 {
		return key[:idx], n
	}
	return key, 0
}

// keysBySerial 返回所有 key 中 serial 部分等于给定 serial 的 key（key 为 serial 或 serial:transportID）
func (m *Manager) keysBySerial(serial string) []string {
	var keys []string
	for k := range m.devices {
		s, _ := parseDeviceKey(k)
		if s == serial {
			keys = append(keys, k)
		}
	}
	return keys
}

// Status 设备状态
type Status string

const (
	StatusOnline  Status = "online"
	StatusOffline Status = "offline"
	StatusBusy    Status = "busy"
)

// Device 设备信息
type Device struct {
	UDID        string    `json:"udid"`         // 设备序列号（adb serial）
	TransportID int       `json:"transport_id"` // adb devices -l 的 transport_id，0 表示未指定
	Status      Status    `json:"status"`       // 设备状态
	Name        string    `json:"name"`         // 设备名称
	Model       string    `json:"model"`        // 设备型号
	OSVersion   string    `json:"os_version"`   // 系统版本
	Battery     int       `json:"battery"`      // 电量百分比
	Resolution  string    `json:"resolution"`   // 分辨率
	LastSeen    time.Time `json:"last_seen"`    // 最后在线时间
	ConnectedAt time.Time `json:"connected_at"` // 连接时间
}

// DialTCPFunc 连接 adb 主机上某地址（如 forward 端口）；走 proxy 时与 adb 命令同路。nil 表示直连。
type DialTCPFunc func(addr string) (net.Conn, error)

// Manager 设备管理器（全量使用 third_party/adb，不再使用 gadb）
type Manager struct {
	devices       map[string]*Device
	adbServer     *adb.Adb           // 唯一 adb 客户端（列表、RunCommand、OpenCommand、Forward 等）
	adbHost       string             // ADB 服务器 host（用于连接 forward 端口，adb 在远端时用此 host）
	adbPort       int                // ADB 服务器 port
	dialTCP       DialTCPFunc        // 可选：连接 adb 主机端口时用（proxy 端点由 endpoint 注入）
	deviceWatcher *adb.DeviceWatcher // 设备状态监控器
	wsHub         interface {        // WebSocket Hub（可选，用于广播设备状态）
		BroadcastDeviceStatus(*Device)
	}
	onDeviceDisconnect func(udid string) // 设备断开时的回调函数（用于清理 WebRTC 连接）
	onConnectionLost   func()            // adb 连接断开时的回调（如 watcher 因错误退出）
	onReconnecting     func()            // 开始重连时回调（retry!=0 时）
	onReconnected      func()            // 重连成功时回调
	onReconnectFailed  func()            // 重连次数用尽仍失败时回调
	retryCount         int               // 断线重试：0 不重试，>0 重试 N 次，<0 一直重试
	mu                 sync.RWMutex
	stopChan           chan struct{} // 停止信号
}

// NewManager 创建设备管理器，使用默认 ADB 地址 127.0.0.1:5037。
func NewManager() *Manager {
	return NewManagerWithAdbAddr("127.0.0.1", 5037)
}

// NewManagerWithAdbAddr 创建设备管理器，指定 ADB 服务器地址（host 为空则 127.0.0.1，port 为 0 则 5037）。
func NewManagerWithAdbAddr(host string, port int) *Manager {
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = adb.AdbPort
	}
	config := adb.ServerConfig{Host: host, Port: port}
	adbServer, err := adb.NewWithConfig(config)
	if err != nil {
		log.Fatalf("错误: 无法创建 ADB 客户端: %v", err)
	}
	if _, err := adbServer.ServerVersion(); err != nil {
		log.Fatalf("错误: ADB 服务器不可用: %v", err)
	}
	logutil.Debugf("[DeviceManager] ADB 已连接 %s:%d", host, port)

	return &Manager{
		devices:   make(map[string]*Device),
		adbServer: adbServer,
		adbHost:   host,
		adbPort:   port,
		stopChan:  make(chan struct{}),
	}
}

// NewManagerWithAdbClient 使用已创建的 ADB 客户端创建设备管理器。dialTCP 可选，proxy 端点时由 endpoint 传入，用于连接 forward 端口。
func NewManagerWithAdbClient(adbClient *adb.Adb, host string, port int, dialTCP DialTCPFunc) *Manager {
	return &Manager{
		devices:   make(map[string]*Device),
		adbServer: adbClient,
		adbHost:   host,
		adbPort:   port,
		dialTCP:   dialTCP,
		stopChan:  make(chan struct{}),
	}
}

// AdbHost 返回 ADB 服务器 host（forward 端口在 adb 所在主机，连接时用此 host）
func (m *Manager) AdbHost() string { return m.adbHost }

// GetDialTCP 返回连接 adb 主机端口用的拨号函数；nil 表示直连（net.Dial）
func (m *Manager) GetDialTCP() DialTCPFunc { return m.dialTCP }

// Stop 停止设备管理器
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 发送停止信号
	select {
	case <-m.stopChan:
		// 已经关闭
	default:
		close(m.stopChan)
	}

	// 停止设备监控
	if m.deviceWatcher != nil {
		m.deviceWatcher.Shutdown()
		m.deviceWatcher = nil
	}
}

// AdbPort 返回 ADB 服务器 port（仅用于显示，连接 forward 端口用 Forward 返回的端口）
func (m *Manager) AdbPort() int { return m.adbPort }

// RegisterDevice 注册设备（存储 key 为 DeviceKey(UDID, TransportID)）
func (m *Manager) RegisterDevice(device *Device) {
	m.mu.Lock()
	device.ConnectedAt = time.Now()
	device.LastSeen = time.Now()
	key := DeviceKey(device.UDID, device.TransportID)
	m.devices[key] = device
	m.mu.Unlock()

	// 广播设备状态变化
	if m.wsHub != nil {
		m.wsHub.BroadcastDeviceStatus(device)
	}
}

// GetDevice 获取设备。key 为 serial 或 serial:transportID
func (m *Manager) GetDevice(key string) (*Device, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	device, exists := m.devices[key]
	return device, exists
}

// GetAllDevices 获取所有设备
func (m *Manager) GetAllDevices() []*Device {
	m.mu.RLock()
	defer m.mu.RUnlock()

	devices := make([]*Device, 0, len(m.devices))
	for _, d := range m.devices {
		devices = append(devices, d)
	}
	return devices
}

// SetOnDeviceDisconnect 设置设备断开时的回调函数
func (m *Manager) SetOnDeviceDisconnect(callback func(udid string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onDeviceDisconnect = callback
}

// SetOnConnectionLost 设置 adb 连接断开时的回调（如 watcher 因错误退出，表示该端点不可用）
func (m *Manager) SetOnConnectionLost(callback func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onConnectionLost = callback
}

// SetRetryCount 设置断线重试：0 不重试，>0 重试 N 次，<0 一直重试；!=0 时断线会重连并触发 OnReconnecting/OnReconnected/OnReconnectFailed
func (m *Manager) SetRetryCount(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retryCount = n
}

// SetOnReconnecting 设置开始重连时的回调
func (m *Manager) SetOnReconnecting(callback func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onReconnecting = callback
}

// SetOnReconnected 设置重连成功时的回调
func (m *Manager) SetOnReconnected(callback func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onReconnected = callback
}

// SetOnReconnectFailed 设置重连失败（次数用尽）时的回调
func (m *Manager) SetOnReconnectFailed(callback func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onReconnectFailed = callback
}

// UpdateDeviceStatus 更新设备状态。key 为 serial 或 serial:transportID
func (m *Manager) UpdateDeviceStatus(key string, status Status) {
	m.mu.Lock()
	var device *Device
	if d, exists := m.devices[key]; exists {
		d.Status = status
		d.LastSeen = time.Now()
		device = d
	}
	m.mu.Unlock()

	// 广播设备状态变化
	if device != nil && m.wsHub != nil {
		m.wsHub.BroadcastDeviceStatus(device)
	}
}

// RemoveDevice 移除设备。key 为 serial 或 serial:transportID
func (m *Manager) RemoveDevice(key string) {
	m.mu.Lock()
	device, exists := m.devices[key]
	if exists {
		delete(m.devices, key)
	}
	m.mu.Unlock()

	// 广播设备移除（通过发送状态为 offline 的设备信息）
	if device != nil && m.wsHub != nil {
		device.Status = StatusOffline
		m.wsHub.BroadcastDeviceStatus(device)
	}
}

// RunInitialDiscovery 同步执行一次设备发现（供添加端点后立即拉取列表，刷新页面即有设备）
func (m *Manager) RunInitialDiscovery() {
	m.discoverDevices()
}

// StartWatcher 仅启动实时设备监控（在后台 goroutine）
func (m *Manager) StartWatcher() {
	m.startDeviceWatcher()
}

// StartDiscovery 启动设备发现（实时监控；ADB 不可用时走断线/重连逻辑，不降级轮询）
func (m *Manager) StartDiscovery() {
	logutil.Debugf("[DeviceManager] 设备发现 goroutine 启动 adb=%s:%d", m.adbHost, m.adbPort)
	m.RunInitialDiscovery()
	go m.StartWatcher()
}

// startDeviceWatcher 启动实时设备监控
func (m *Manager) startDeviceWatcher() {
	if m.adbServer == nil {
		logutil.Warnf("[DeviceManager] ADB服务器未初始化")
		m.invokeConnectionLostAndMaybeReconnect()
		return
	}

	// 先验证 ADB 服务器是否可用（不可用则走断线/重连逻辑，不降级轮询）
	_, err := m.adbServer.ServerVersion()
	if err != nil {
		logutil.Warnf("[DeviceManager] ADB服务器不可用: %v", err)
		m.invokeConnectionLostAndMaybeReconnect()
		return
	}

	watcher := m.adbServer.NewDeviceWatcher()
	m.deviceWatcher = watcher

	logutil.Debugf("[DeviceManager] 实时设备监控已启动 adb=%s:%d", m.adbHost, m.adbPort)

	go func() {
		defer watcher.Shutdown()

		for {
			select {
			case <-m.stopChan:
				logutil.Debugf("[DeviceManager] 停止设备监控")
				return
			case event, ok := <-watcher.C():
				if !ok {
					if err := watcher.Err(); err != nil {
						logutil.Errorf("[DeviceManager] 设备监控错误（adb 连接已断开）: %v", err)
						m.invokeConnectionLostAndMaybeReconnect()
					}
					return
				}

				// 处理设备状态变化事件
				m.handleDeviceStateChange(event)
			}
		}
	}()
}

// invokeConnectionLostAndMaybeReconnect ADB 不可用或 watcher 断连时调用：按 retry 重连（<0 一直重试，>0 重试 N 次），失败则触发 onReconnectFailed，最后触发 onConnectionLost
func (m *Manager) invokeConnectionLostAndMaybeReconnect() {
	m.mu.RLock()
	retryN := m.retryCount
	onReconnecting := m.onReconnecting
	onReconnected := m.onReconnected
	onReconnectFailed := m.onReconnectFailed
	onLost := m.onConnectionLost
	m.mu.RUnlock()
	if retryN != 0 && (onReconnecting != nil || onReconnected != nil || onReconnectFailed != nil) {
		if onReconnecting != nil {
			onReconnecting()
		}
		attempt := 0
		for {
			if attempt > 0 {
				time.Sleep(3 * time.Second)
			}
			if _, errV := m.adbServer.ServerVersion(); errV != nil {
				attempt++
				if retryN < 0 {
					logutil.Debugf("[DeviceManager] 重连尝试 %d 失败: %v", attempt, errV)
				} else {
					logutil.Debugf("[DeviceManager] 重连尝试 %d/%d 失败: %v", attempt, retryN, errV)
				}
				if retryN > 0 && attempt >= retryN {
					break
				}
				continue
			}
			m.startDeviceWatcher()
			if onReconnected != nil {
				onReconnected()
			}
			return
		}
		if retryN > 0 {
			logutil.Warnf("[DeviceManager] 重连 %d 次后仍失败", retryN)
		}
		if onReconnectFailed != nil {
			onReconnectFailed()
		}
	}
	if onLost != nil {
		onLost()
	}
}

// handleDeviceStateChange 处理设备状态变化（event.Serial 为 adb serial，可能对应多个 key）
func (m *Manager) handleDeviceStateChange(event adb.DeviceStateChangedEvent) {
	serial := event.Serial

	logutil.Debugf("[DeviceManager] 设备状态变化: %s, 旧状态: %s, 新状态: %s",
		serial, event.OldState.String(), event.NewState.String())

	m.mu.RLock()
	keys := m.keysBySerial(serial)
	onDisconnect := m.onDeviceDisconnect
	m.mu.RUnlock()

	switch event.NewState {
	case adb.StateOnline:
		if len(keys) == 0 {
			logutil.Infof("[DeviceManager] 发现新设备（上线）: %s", serial)
			m.discoverAndroidDeviceByUDID(serial)
		} else {
			for _, key := range keys {
				m.UpdateDeviceStatus(key, StatusOnline)
			}
			logutil.Infof("[DeviceManager] 设备重新上线: %s", serial)
		}

	case adb.StateOffline:
		for _, key := range keys {
			if d, ok := m.GetDevice(key); ok {
				logutil.Infof("[DeviceManager] 设备离线: %s (%s)", d.Name, key)
			}
			m.UpdateDeviceStatus(key, StatusOffline)
			if onDisconnect != nil {
				onDisconnect(key)
			}
		}

	case adb.StateDisconnected:
		for _, key := range keys {
			device, _ := m.GetDevice(key)
			m.RemoveDevice(key)
			if onDisconnect != nil {
				onDisconnect(key)
			}
			if device != nil {
				logutil.Infof("[DeviceManager] 设备断开连接并移除: %s (%s)", device.Name, key)
				if m.wsHub != nil {
					device.Status = StatusOffline
					m.wsHub.BroadcastDeviceStatus(device)
				}
			}
		}

	case adb.StateUnauthorized:
		for _, key := range keys {
			if d, ok := m.GetDevice(key); ok {
				logutil.Warnf("[DeviceManager] 设备未授权: %s (%s)", d.Name, key)
			}
			m.UpdateDeviceStatus(key, StatusOffline)
		}
		if len(keys) == 0 {
			logutil.Warnf("[DeviceManager] 发现未授权设备: %s", serial)
		}

	default:
		logutil.Warnf("[DeviceManager] 设备状态未知: %s, 状态: %s", serial, event.NewState.String())
	}
}

// discoverAndroidDeviceByUDID 根据 serial 发现 Android 设备（ListDevices -l 可能返回同 serial 多条，按 transport_id 分别注册）
func (m *Manager) discoverAndroidDeviceByUDID(serial string) {
	if m.adbServer == nil {
		return
	}
	list, err := m.adbServer.ListDevices()
	if err != nil {
		return
	}
	for _, info := range list {
		if info.Serial != serial || info.Status != "device" {
			continue
		}
		key := DeviceKey(serial, info.TransportID)
		m.mu.RLock()
		_, exists := m.devices[key]
		m.mu.RUnlock()
		if exists {
			m.UpdateDeviceStatus(key, StatusOnline)
			continue
		}
		adbDevice := m.adbDeviceForSerialTransport(serial, info.TransportID)
		device := m.getAndroidDeviceInfo(adbDevice, serial, info.TransportID)
		if device != nil {
			m.RegisterDevice(device)
			logutil.Infof("[DeviceManager] 发现新设备: %s (%s)", device.Name, key)
		}
	}
}

// StopDiscovery 停止设备发现
func (m *Manager) StopDiscovery() {
	close(m.stopChan)
	if m.deviceWatcher != nil {
		m.deviceWatcher.Shutdown()
	}
}

// discoverDevices 发现设备
func (m *Manager) discoverDevices() {
	m.discoverAndroidDevices()
}

// discoverAndroidDevices 发现 Android 设备（初始列表与轮询用，使用 ListDevices -l 含 transport_id）
func (m *Manager) discoverAndroidDevices() {
	if m.adbServer == nil {
		logutil.Warnf("[DeviceManager] ListDevices 跳过: adbServer 为 nil")
		return
	}
	logutil.Debugf("[DeviceManager] ListDevices 开始 adb=%s:%d", m.adbHost, m.adbPort)
	list, err := m.adbServer.ListDevices()
	if err != nil {
		logutil.Warnf("[DeviceManager] ListDevices 失败: %v", err)
		return
	}
	if len(list) == 0 {
		logutil.Debugf("[DeviceManager] ListDevices 返回 0 台设备 (adb=%s:%d)", m.adbHost, m.adbPort)
	} else {
		logutil.Debugf("[DeviceManager] ListDevices 返回 %d 台设备 adb=%s:%d", len(list), m.adbHost, m.adbPort)
	}
	for _, info := range list {
		if info.Status != "device" {
			logutil.Debugf("[DeviceManager] 设备 %s 状态=%s 跳过", info.Serial, info.Status)
			continue
		}
		key := DeviceKey(info.Serial, info.TransportID)
		m.mu.RLock()
		_, exists := m.devices[key]
		m.mu.RUnlock()
		if !exists {
			adbDevice := m.adbDeviceForSerialTransport(info.Serial, info.TransportID)
			device := m.getAndroidDeviceInfo(adbDevice, info.Serial, info.TransportID)
			if device != nil {
				m.RegisterDevice(device)
				logutil.Infof("[DeviceManager] 发现设备: %s (%s)", device.Name, key)
			} else {
				logutil.Warnf("[DeviceManager] 获取设备 %s 详情失败(getprop 等)，已跳过", key)
			}
		} else {
			m.UpdateDeviceStatus(key, StatusOnline)
		}
	}
}

// adbDeviceForSerialTransport 返回用于该 serial/transportID 的 *adb.Device。
// 对应 adb 命令行：-t ID 时用 host-transport-id:ID，-s SERIAL 时用 host-serial:SERIAL；
// 调用方应传入 parseDeviceKey(deviceKey) 得到的 serial 与 transportID，勿把 "serial:transportID" 整串当 serial 传入。
func (m *Manager) adbDeviceForSerialTransport(serial string, transportID int) *adb.Device {
	if m.adbServer == nil {
		return nil
	}
	if transportID > 0 {
		return m.adbServer.Device(adb.DeviceWithTransportID(transportID))
	}
	return m.adbServer.Device(adb.DeviceWithSerial(serial))
}

// getAndroidDeviceInfo 获取 Android 设备详细信息（transportID 来自 adb devices -l）
func (m *Manager) getAndroidDeviceInfo(adbDevice *adb.Device, serial string, transportID int) *Device {
	device := &Device{
		UDID:        serial,
		TransportID: transportID,
		Status:      StatusOnline,
	}
	run := func(cmd string, args ...string) (string, error) { return adbDevice.RunCommand(cmd, args...) }

	if model, err := run("getprop", "ro.product.model"); err == nil {
		device.Model = strings.TrimSpace(model)
		device.Name = device.Model
	}
	if brand, err := run("getprop", "ro.product.brand"); err == nil {
		brandStr := strings.TrimSpace(brand)
		if device.Name == "" {
			device.Name = brandStr
		} else {
			device.Name = brandStr + " " + device.Name
		}
	}
	if version, err := run("getprop", "ro.build.version.release"); err == nil {
		device.OSVersion = strings.TrimSpace(version)
	}
	if size, err := run("wm", "size"); err == nil {
		sizeStr := strings.TrimSpace(size)
		if strings.HasPrefix(sizeStr, "Physical size: ") {
			device.Resolution = strings.TrimPrefix(sizeStr, "Physical size: ")
		}
	}
	if battery, err := run("dumpsys", "battery"); err == nil {
		for _, line := range strings.Split(battery, "\n") {
			if strings.Contains(line, "level: ") {
				device.Battery = 100
				break
			}
		}
	}
	return device
}

// SetWSHub 设置 WebSocket Hub（用于广播设备状态变化）
func (m *Manager) SetWSHub(wsHub interface {
	BroadcastDeviceStatus(*Device)
}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wsHub = wsHub
}
