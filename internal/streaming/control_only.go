package streaming

import (
	"embed"
	"sync"

	"github.com/ms-robots/ms-robot/internal/logutil"
	adb "github.com/ms-robots/ms-robot/third_party/adb"
)

// DeviceScrcpyPool 按设备缓存的常驻 scrcpy 实例（与 u2 类似：启动后一直持有；仅开启投屏时读视频流，关闭投屏只停读流，scrcpy 不退出）
type DeviceScrcpyPool struct {
	mu      sync.Mutex
	servers map[string]*ScrcpyServer // deviceKey -> server
}

// NewDeviceScrcpyPool 创建设备 scrcpy 池
func NewDeviceScrcpyPool() *DeviceScrcpyPool {
	return &DeviceScrcpyPool{servers: make(map[string]*ScrcpyServer)}
}

// Get 返回池中该设备的 scrcpy 实例（仅当已存在且 running 时返回 true）。不启动新实例。
func (p *DeviceScrcpyPool) Get(deviceKey string) (*ScrcpyServer, bool) {
	p.mu.Lock()
	s := p.servers[deviceKey]
	p.mu.Unlock()
	if s == nil || !s.IsRunning() {
		return nil, false
	}
	return s, true
}

// GetOrStart 获取或启动该设备的 scrcpy。若已存在且 running 则直接返回。maxSize、videoBitRate、controlOnly 仅在新启动时生效。controlOnly 为 true 时仅控制（不拉视频/音频）；否则投屏（video+control，不采音频；音视频分离后续再做）。
func (p *DeviceScrcpyPool) GetOrStart(deviceKey string, adbDevice *adb.Device, adbClient *adb.Adb, deviceUDID, adbHost string, dialTCP dialTCPFunc, assetsFS embed.FS, maxSize, videoBitRate int, controlOnly bool) (*ScrcpyServer, error) {
	p.mu.Lock()
	s := p.servers[deviceKey]
	if s != nil {
		s.mu.Lock()
		running := s.running
		serverControlOnly := s.controlOnly
		s.mu.Unlock()
		p.mu.Unlock()
		if running {
			// 投屏需要视频；若当前是仅控制实例则替换为带视频的
			if !controlOnly && serverControlOnly {
				p.mu.Lock()
				delete(p.servers, deviceKey)
				p.mu.Unlock()
				s.Stop()
			} else {
				return s, nil
			}
		} else {
			p.mu.Lock()
			delete(p.servers, deviceKey)
			p.mu.Unlock()
		}
	} else {
		p.mu.Unlock()
	}

	server, err := NewScrcpyServer(adbDevice, adbClient, deviceUDID, assetsFS, adbHost, dialTCP)
	if err != nil {
		return nil, err
	}
	if controlOnly {
		server.SetControlOnly(true)
	} else {
		if maxSize > 0 {
			server.SetMaxSize(maxSize)
		}
		if videoBitRate > 0 {
			server.SetVideoBitRate(videoBitRate)
		}
		// 常驻模式：开音频连接，由控制命令动态启停采集
		server.SetEnableAudio(true)
	}
	// 不在此处 Start：须待 AndroidStreamer.Start 内紧挨 StartReading，避免视频无人消费导致老设备 Abort。
	p.mu.Lock()
	p.servers[deviceKey] = server
	p.mu.Unlock()
	logutil.Debugf("[scrcpy-pool] 设备 %s: scrcpy 实例已入池，将在首次投屏 Start 时连接设备", deviceKey)
	return server, nil
}

// Remove 移除并停止该设备的 scrcpy（如设备断开时由上层调用）
func (p *DeviceScrcpyPool) Remove(deviceKey string) {
	p.mu.Lock()
	s := p.servers[deviceKey]
	delete(p.servers, deviceKey)
	p.mu.Unlock()
	if s != nil {
		s.Stop()
		logutil.Debugf("[scrcpy-pool] 设备 %s 已停止 scrcpy", deviceKey)
	}
}
