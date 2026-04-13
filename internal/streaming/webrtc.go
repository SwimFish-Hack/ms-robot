package streaming

import (
	"embed"
	"fmt"
	devicemgr "github.com/ms-robots/ms-robot/internal/device"
	"github.com/ms-robots/ms-robot/internal/logutil"
	"github.com/ms-robots/ms-robot/third_party/adb"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v3"
)

const maxPendingICECandidates = 32

// scrcpyKeyFrameSettleDelay：WebRTC 已 Connected 且本机 scrcpy 已跑起来后，再稍等片刻再发 0x11，降低设备端 SurfaceCapture 未就绪时 resetVideo NPE 的概率。
const scrcpyKeyFrameSettleDelay = 350 * time.Millisecond

// pendingICEList 每会话一桶：sync.Map 管 key；桶内一把互斥锁保护切片（多路 trickle ICE 并发 append + Offer 协程 take）。
// 不用 chan：这里是「多写一读、一次性整表 drain」，mutex+slice 比再开 goroutine 或关 chan 更简单清晰。
type pendingICEList struct {
	mu   sync.Mutex
	list []webrtc.ICECandidateInit
}

func newPendingICEList() *pendingICEList {
	return &pendingICEList{}
}

func (p *pendingICEList) appendIce(c webrtc.ICECandidateInit) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.list) < maxPendingICECandidates {
		p.list = append(p.list, c)
	}
}

func (p *pendingICEList) take() []webrtc.ICECandidateInit {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.list
	p.list = nil
	return out
}

// WebRTCManager WebRTC流管理器（使用 sync.Map，无 mutex）
type WebRTCManager struct {
	streams           sync.Map // device_udid string -> *sync.Map (sessionID -> *StreamSession)
	deviceStreamers   sync.Map // device_udid string -> *AndroidStreamer
	streamSupervisors sync.Map // device_udid -> *streamSupervisor（每设备一条 goroutine：ctrl 与 framesIn select 串行处理控制面；H264 每会话独立 worker）
	// pendingICECandidates：sync.Map 按会话分 key；值 *pendingICEList 内 sync.Mutex 护切片
	pendingICECandidates sync.Map // key: pendingICECandidatesKey(streamKey,sessionID) -> *pendingICEList
	unifiedDeviceManager *devicemgr.UnifiedDeviceManager
	scrcpyPool           *DeviceScrcpyPool // 常驻 scrcpy 池，与 display-power 共用；nil 则 streamer 自建 scrcpy
	assetsFS             embed.FS
	stunServers          []string
	turnServers          []string
	localTurnServer      *TurnServer
}

// StreamSession 流会话
type StreamSession struct {
	Device        *devicemgr.Device
	PeerConn      *webrtc.PeerConnection
	VideoTrack    *webrtc.TrackLocalStaticSample
	AudioTrack    *webrtc.TrackLocalStaticSample // Opus 设备音频
	DataChannel   *webrtc.DataChannel            // 数据通道，用于发送控制命令
	adbDevice     *adb.Device
	streamer      *AndroidStreamer          // 共享的streamer（同一设备的所有会话共享）
	sessionID     string                    // 会话ID（用于区分不同的WebRTC连接）
	iceCandidates []webrtc.ICECandidateInit // 存储ICE候选（在设置本地描述后发送给前端）
	// 首连时 scrcpy 在独立 goroutine 中 Start()；后台 <-scrcpyConnectDone 后再 supervisor.registerVideoPeer，与 SDP 返回无硬同步
	scrcpyConnectDone chan error
	mu                sync.Mutex
	keyframeOnConnect sync.Once // Connected 瞬间若 scrcpy 已就绪则经短延迟要 IDR；否则依赖后续码流自然关键帧
}

// GetStreamer 返回会话使用的 AndroidStreamer（供 API 等包使用）
func (s *StreamSession) GetStreamer() *AndroidStreamer {
	return s.streamer
}

func (m *WebRTCManager) requestScrcpyKeyFrame(streamKey string) {
	v, ok := m.deviceStreamers.Load(streamKey)
	if !ok {
		return
	}
	st := v.(*AndroidStreamer)
	srv := st.GetScrcpyServer()
	if srv == nil {
		return
	}
	go func() {
		if err := srv.RequestKeyFrame(); err != nil {
			logutil.Warnf("[WebRTC] 设备 %s: 连通后请求关键帧失败: %v", streamKey, err)
		} else {
			logutil.Debugf("[WebRTC] 设备 %s: PeerConnection 已连通，已请求关键帧", streamKey)
		}
	}()
}

// requestScrcpyKeyFrameWhenScrcpyReady：仅当 WebRTC Connected 时 scrcpy 已在出流（AndroidStreamer.IsRunning，即 Start 已跑完）才发 RESET_VIDEO。
// 若 ICE 早于 scrcpy 就绪，则不发送——随后挂轨收到的码流自含关键帧/GOP，无需 0x11。
func (m *WebRTCManager) requestScrcpyKeyFrameWhenScrcpyReady(streamKey string, session *StreamSession) {
	st := session.streamer
	if st == nil || !st.IsRunning() {
		return
	}
	srv := st.GetScrcpyServer()
	if srv == nil || !srv.IsRunning() {
		return
	}
	pc := session.PeerConn
	go func() {
		time.Sleep(scrcpyKeyFrameSettleDelay)
		if pc != nil && pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
			return
		}
		m.requestScrcpyKeyFrame(streamKey)
	}()
}

var globalWebRTCManager *WebRTCManager
var once sync.Once

// GetWebRTCManager 获取全局WebRTC管理器
func GetWebRTCManager() *WebRTCManager {
	once.Do(func() {
		globalWebRTCManager = &WebRTCManager{}
	})
	return globalWebRTCManager
}

// SetAssetsFS 设置嵌入的文件系统（仅启动时调用）
func (m *WebRTCManager) SetAssetsFS(assetsFS embed.FS) {
	m.assetsFS = assetsFS
}

// SetScrcpyPool 设置常驻 scrcpy 池（与 display-power 共用；仅启动时调用）
func (m *WebRTCManager) SetScrcpyPool(pool *DeviceScrcpyPool) {
	m.scrcpyPool = pool
}

// SetICEServers 设置ICE服务器（仅启动时调用）
func (m *WebRTCManager) SetICEServers(stunServers, turnServers []string) {
	m.stunServers = stunServers
	m.turnServers = turnServers
}

// SetLocalTurnServer 设置内置TURN服务器（仅启动时调用）
func (m *WebRTCManager) SetLocalTurnServer(turnServer *TurnServer) {
	m.localTurnServer = turnServer
}

// GetICEServers 获取ICE服务器配置（用于前端）。配置仅在启动时设置，只读无需加锁。
func (m *WebRTCManager) GetICEServers() ([]string, []string) {
	return m.stunServers, m.turnServers
}

// GetLocalTurnServer 获取内置TURN服务器。配置仅在启动时设置，只读无需加锁。
func (m *WebRTCManager) GetLocalTurnServer() *TurnServer {
	return m.localTurnServer
}

// buildICEServers 合并 STUN、内置 TURN、turnServers（含 URL 中 username/credential 解析）。custom 非空则整表覆盖。
// logLabel 非空时在选用 custom 或最终列表为空告警时打印日志。
func (m *WebRTCManager) buildICEServers(custom []webrtc.ICEServer, logLabel string) []webrtc.ICEServer {
	out := make([]webrtc.ICEServer, 0, len(m.stunServers)+len(m.turnServers)+2)
	for _, stunURL := range m.stunServers {
		if stunURL != "" {
			out = append(out, webrtc.ICEServer{URLs: []string{stunURL}})
		}
	}
	if m.localTurnServer != nil {
		turnURL := m.localTurnServer.GetURL("127.0.0.1")
		out = append(out, webrtc.ICEServer{
			URLs:       []string{turnURL},
			Username:   m.localTurnServer.GetUsername(),
			Credential: m.localTurnServer.GetPassword(),
		})
		logutil.Debugf("[WebRTC] 添加内置TURN服务器: %s", turnURL)
	}
	for _, turnURL := range m.turnServers {
		if turnURL == "" {
			continue
		}
		server := webrtc.ICEServer{URLs: []string{turnURL}}
		if strings.Contains(turnURL, "username=") && strings.Contains(turnURL, "credential=") {
			parts := strings.Split(turnURL, "?")
			if len(parts) == 2 {
				baseURL, params := parts[0], parts[1]
				var username, credential string
				for _, param := range strings.Split(params, "&") {
					if strings.HasPrefix(param, "username=") {
						username = strings.TrimPrefix(param, "username=")
					} else if strings.HasPrefix(param, "credential=") {
						credential = strings.TrimPrefix(param, "credential=")
					}
				}
				if username != "" && credential != "" {
					server.Username = username
					server.Credential = credential
					server.URLs = []string{baseURL}
				}
			}
		}
		out = append(out, server)
	}
	if len(custom) > 0 {
		if logLabel != "" {
			logutil.Debugf("[WebRTC] 设备 %s: 使用传入的自定义ICE服务器 (%d 个)", logLabel, len(custom))
		}
		return custom
	}
	if len(out) == 0 {
		logutil.Warnf("[WebRTC] 警告: 没有配置任何ICE服务器，WebRTC连接可能失败")
	}
	return out
}

// activeStreamerOr 挂轨前从全局表取当前 streamer（重连后可能已换新实例）；表无记录时用 fallback（CreateStream 本地变量）。
func (m *WebRTCManager) activeStreamerOr(streamKey string, fallback *AndroidStreamer) *AndroidStreamer {
	if v, ok := m.deviceStreamers.Load(streamKey); ok {
		return v.(*AndroidStreamer)
	}
	return fallback
}

// getOrCreateStreamer 获取或创建该设备的共享 AndroidStreamer（与 CreateStream 复用同一 stream）。streamKey 为空时用 device.UDID。
func (m *WebRTCManager) getOrCreateStreamer(device *devicemgr.Device, streamKey string, maxSize, videoBitRate int) (*AndroidStreamer, string, error) {
	streamMapKey := device.UDID
	if streamKey != "" {
		streamMapKey = streamKey
	}
	deviceKey := devicemgr.DeviceKey(device.UDID, device.TransportID)

	var sharedStreamer *AndroidStreamer
	if v, ok := m.deviceStreamers.Load(streamMapKey); ok {
		sharedStreamer = v.(*AndroidStreamer)
	}
	if sharedStreamer == nil {
		unifiedMgr := m.unifiedDeviceManager
		adbDevice, endpointAddr, err := unifiedMgr.GetADBDevice(deviceKey)
		if err != nil {
			return nil, "", fmt.Errorf("获取ADB设备失败: %v", err)
		}
		var activeDeviceManager *devicemgr.Manager
		if endpointAddr != "" {
			if mgr, ok := unifiedMgr.GetManagerForEndpoint(endpointAddr); ok {
				activeDeviceManager = mgr
			}
		}
		if activeDeviceManager == nil {
			return nil, "", fmt.Errorf("无法获取设备 %s 的端点管理器", streamMapKey)
		}
		var scrcpyServer *ScrcpyServer
		if m.scrcpyPool != nil {
			var dialTCP dialTCPFunc
			if dmDial := activeDeviceManager.GetDialTCP(); dmDial != nil {
				dialTCP = func(addr string) (net.Conn, error) { return dmDial(addr) }
			}
			var err error
			scrcpyServer, err = m.scrcpyPool.GetOrStart(deviceKey, adbDevice, activeDeviceManager.AdbClient(), device.UDID, activeDeviceManager.AdbHost(), dialTCP, m.assetsFS, maxSize, videoBitRate, false)
			if err != nil {
				return nil, "", fmt.Errorf("获取或启动 scrcpy 失败: %v", err)
			}
		}
		streamer, err := NewAndroidStreamer(activeDeviceManager, device, adbDevice, m.assetsFS, scrcpyServer)
		if err != nil {
			return nil, "", fmt.Errorf("创建AndroidStreamer失败: %v", err)
		}
		if scrcpyServer == nil {
			streamer.SetScrcpyOptions(maxSize, videoBitRate)
		}
		streamer.SetOnDisconnect(func() {
			if _, ok := m.deviceStreamers.Load(streamMapKey); ok {
				m.handleDeviceDisconnect(streamMapKey)
			}
		})
		if v, loaded := m.deviceStreamers.LoadOrStore(streamMapKey, streamer); loaded {
			sharedStreamer = v.(*AndroidStreamer)
			streamer.Stop()
		} else {
			sharedStreamer = streamer
		}
	}
	return sharedStreamer, streamMapKey, nil
}

// CreateStream 为设备创建WebRTC流（无锁，使用 sync.Map）。streamKey 为多端点时的存储 key（deviceKey@endpointID），空则用 device.UDID。音视频分离后续再做，当前仅视频+控制。
// 首连：scrcpy 与 WebRTC 栈构建并行——独立 goroutine 执行 Start()，经 session.scrcpyConnectDone 与 Offer/Answer 侧汇合后再挂轨、SetLocalDescription。
func (m *WebRTCManager) CreateStream(device *devicemgr.Device, customICEServers []webrtc.ICEServer, maxSize, videoBitRate int, streamKey string) (session *StreamSession, err error) {
	deviceKey := devicemgr.DeviceKey(device.UDID, device.TransportID)
	sharedStreamer, streamMapKey, err := m.getOrCreateStreamer(device, streamKey, maxSize, videoBitRate)
	if err != nil {
		return nil, err
	}
	logutil.Debugf("[WebRTC] 设备 %s: 创建新的流会话...", streamMapKey)

	sup := m.supervisorFor(streamMapKey)
	res := sup.webRTCReserve(sharedStreamer)
	scrcpyWait := res.scrcpyWait
	first := res.first
	if first {
		logutil.Debugf("[WebRTC] 设备 %s: 首连，经 supervisor 启动 scrcpy（与 PeerConnection 并行；挂轨在 SDP 汇合点后）", streamMapKey)
	}
	defer func() {
		if err == nil {
			return
		}
		sup.tryWebRTCRelease()
	}()

	iceServers := m.buildICEServers(customICEServers, device.UDID)

	config := webrtc.Configuration{
		ICEServers:         iceServers,
		ICETransportPolicy: webrtc.ICETransportPolicyAll, // 允许使用STUN和TURN，优先直连
	}

	peerConn, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, fmt.Errorf("创建PeerConnection失败: %v", err)
	}

	// 创建视频轨道（使用H.264编码，浏览器支持更好）
	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video",
		"pion",
	)
	if err != nil {
		peerConn.Close()
		return nil, fmt.Errorf("创建视频轨道失败: %v", err)
	}
	if _, err = peerConn.AddTrack(videoTrack); err != nil {
		peerConn.Close()
		return nil, fmt.Errorf("添加视频轨道失败: %v", err)
	}

	// 默认添加 Opus 音频轨；是否采集/转发由「启用音频」API 控制
	audioTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio",
		"pion",
	)
	if err != nil {
		peerConn.Close()
		return nil, fmt.Errorf("创建音频轨道失败: %v", err)
	}
	if _, err = peerConn.AddTrack(audioTrack); err != nil {
		peerConn.Close()
		return nil, fmt.Errorf("添加音频轨道失败: %v", err)
	}

	// 获取ADB设备对象（用于会话）
	adbDevice, _, err := m.unifiedDeviceManager.GetADBDevice(deviceKey)
	if err != nil {
		peerConn.Close()
		return nil, fmt.Errorf("获取ADB设备失败: %v", err)
	}

	sessionID := device.UDID + "_" + uuid.New().String()

	// 创建DataChannel用于控制命令（标签为"control"）
	// 注意：必须在设置本地描述之前创建，这样DataChannel信息才会包含在Answer的SDP中
	logutil.Debugf("[WebRTC] 设备 %s 会话 %s: 创建DataChannel (label: control)...", device.UDID, sessionID)

	// 配置DataChannel选项
	ordered := true
	dataChannelInit := &webrtc.DataChannelInit{
		Ordered: &ordered, // 有序传输
	}

	dataChannel, err := peerConn.CreateDataChannel("control", dataChannelInit)
	if err != nil {
		logutil.Errorf("[WebRTC] 设备 %s 会话 %s: 创建DataChannel失败: %v", streamMapKey, sessionID, err)
		peerConn.Close()
		return nil, fmt.Errorf("创建DataChannel失败: %v", err)
	}
	logutil.Debugf("[WebRTC] 设备 %s 会话 %s: DataChannel已创建 (ID: %d, Label: %s, ReadyState: %s)",
		device.UDID, sessionID, dataChannel.ID(), dataChannel.Label(), dataChannel.ReadyState().String())

	// 设置DataChannel事件处理
	dataChannel.OnOpen(func() {
		logutil.Debugf("[WebRTC] 设备 %s 会话 %s: DataChannel已打开 (ReadyState: %s)",
			device.UDID, sessionID, dataChannel.ReadyState().String())
	})

	dataChannel.OnClose(func() {
		logutil.Debugf("[WebRTC] 设备 %s 会话 %s: DataChannel已关闭", device.UDID, sessionID)
	})

	dataChannel.OnError(func(err error) {
		logutil.Debugf("[WebRTC] 设备 %s 会话 %s: DataChannel错误: %v", device.UDID, sessionID, err)
	})

	// 监听DataChannel消息（控制命令）
	dataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
		m.handleControlMessage(streamMapKey, sessionID, msg.Data)
	})

	session = &StreamSession{
		Device:            device,
		PeerConn:          peerConn,
		VideoTrack:        videoTrack,
		AudioTrack:        audioTrack,
		DataChannel:       dataChannel,
		adbDevice:         adbDevice,
		streamer:          sharedStreamer, // 使用共享的streamer
		sessionID:         sessionID,
		iceCandidates:     make([]webrtc.ICECandidateInit, 0),
		scrcpyConnectDone: scrcpyWait,
	}

	// 监听ICE候选生成事件（在设置本地描述之前就开始收集）
	peerConn.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			// candidate == nil 表示ICE候选收集完成
			logutil.Debugf("[WebRTC] 设备 %s 会话 %s: ICE候选收集完成", device.UDID, sessionID)
			return
		}

		// 将ICE候选添加到会话的候选列表中
		session.mu.Lock()
		session.iceCandidates = append(session.iceCandidates, candidate.ToJSON())
		session.mu.Unlock()

		logutil.Debugf("[WebRTC] 设备 %s 会话 %s: 生成ICE候选: %s (类型: %s)",
			device.UDID, sessionID, candidate.Address, candidate.Typ.String())
	})

	// 监听连接状态：Disconnected 常可恢复，勿在此处摘会话以免误伤多路并发；Closed/Failed 再摘。
	peerConn.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		logutil.Debugf("[WebRTC] 设备 %s 会话 %s 连接状态: %s", device.UDID, sessionID, s.String())
		if s == webrtc.PeerConnectionStateConnected {
			session.keyframeOnConnect.Do(func() {
				m.requestScrcpyKeyFrameWhenScrcpyReady(streamMapKey, session)
			})
		}
		if s == webrtc.PeerConnectionStateClosed || s == webrtc.PeerConnectionStateFailed {
			logutil.Debugf("[WebRTC] 设备 %s 会话 %s 检测到连接结束 (状态: %s)，移除会话", streamMapKey, sessionID, s.String())
			m.removeStreamSession(streamMapKey, sessionID)
		}
	})

	// ICE：仅 Failed 时摘会话。Disconnected 多为瞬时，摘会话会导致多标签/弱网下误断。
	peerConn.OnICEConnectionStateChange(func(iceState webrtc.ICEConnectionState) {
		logutil.Debugf("[WebRTC] 设备 %s 会话 %s ICE连接状态: %s", device.UDID, sessionID, iceState.String())
		if iceState == webrtc.ICEConnectionStateFailed {
			logutil.Debugf("[WebRTC] 设备 %s 会话 %s ICE Failed，通知 supervisor 移除会话", streamMapKey, sessionID)
			m.removeStreamSession(streamMapKey, sessionID)
		}
	})

	// 存储会话（sync.Map: deviceUDID -> *sync.Map(sessionID -> *StreamSession)）
	sessionsVal, _ := m.streams.LoadOrStore(streamMapKey, &sync.Map{})
	sessionsMap := sessionsVal.(*sync.Map)
	sessionsMap.Store(sessionID, session)

	logutil.Debugf("[WebRTC] 设备 %s: WebRTC 会话已登记（streamSupervisor 维护会话表与摘牌）", streamMapKey)

	// 首连时 streamer 可能仍在异步 Start()，挂轨推迟到 HandleWebRTCOfferResolved 中 <-scrcpyConnectDone 之后
	if scrcpyWait == nil {
		regSt := m.activeStreamerOr(streamMapKey, sharedStreamer)
		if err = sup.registerVideoPeer(regSt, sessionID, videoTrack, peerConn); err != nil {
			sessionsMap.Delete(sessionID)
			peerConn.Close()
			err = fmt.Errorf("添加视频轨道失败: %v", err)
			return nil, err
		}
		if audioTrack != nil {
			sharedStreamer.AddAudioTrack(sessionID, audioTrack)
		}
		logutil.Debugf("[WebRTC] 设备 %s: 视频轨道已添加 (sessionID: %s)", streamMapKey, sessionID)
	}

	return session, nil
}

// GetStream 获取设备的流会话（返回第一个会话）
func (m *WebRTCManager) GetStream(deviceUDID string) (*StreamSession, bool) {
	v, ok := m.streams.Load(deviceUDID)
	if !ok {
		return nil, false
	}
	sessionsMap := v.(*sync.Map)
	var first *StreamSession
	sessionsMap.Range(func(_, s interface{}) bool {
		first = s.(*StreamSession)
		return false
	})
	return first, first != nil
}

// GetStreamBySessionID 根据会话ID获取流会话
func (m *WebRTCManager) GetStreamBySessionID(deviceUDID, sessionID string) (*StreamSession, bool) {
	v, ok := m.streams.Load(deviceUDID)
	if !ok {
		return nil, false
	}
	s, ok := v.(*sync.Map).Load(sessionID)
	if !ok {
		return nil, false
	}
	return s.(*StreamSession), true
}

// GetStreamSessions 获取设备的所有流会话
func (m *WebRTCManager) GetStreamSessions(deviceUDID string) (map[string]*StreamSession, bool) {
	v, ok := m.streams.Load(deviceUDID)
	if !ok {
		return nil, false
	}
	result := make(map[string]*StreamSession)
	v.(*sync.Map).Range(func(k, s interface{}) bool {
		result[k.(string)] = s.(*StreamSession)
		return true
	})
	return result, true
}

// closeAllSessionsPeerConns 关闭某 streamKey 下所有会话的 PeerConnection。
// asyncClose=false：同步 Close，用于无 supervisor 的兜底、RemoveStream 等与 map 删除的顺序要求。
// asyncClose=true：每路 go Close，仅允许在 streamSupervisor 处理 msgDeviceDisconnect 时调用，避免 pion Close 同步回调再向同一条 ctrl 投递导致死锁。
func (m *WebRTCManager) closeAllSessionsPeerConns(streamKey string, asyncClose bool) {
	v, ok := m.streams.Load(streamKey)
	if !ok {
		return
	}
	v.(*sync.Map).Range(func(_, s interface{}) bool {
		if session := s.(*StreamSession); session.PeerConn != nil {
			pc := session.PeerConn
			if asyncClose {
				go func() { _ = pc.Close() }()
			} else {
				_ = pc.Close()
			}
		}
		return true
	})
}

// handleDeviceDisconnect 处理设备断开：经 streamSupervisor 串行 teardown
func (m *WebRTCManager) handleDeviceDisconnect(deviceUDID string) {
	logutil.Debugf("[WebRTC] 设备 %s: 处理设备断开", deviceUDID)
	if v, ok := m.streamSupervisors.Load(deviceUDID); ok {
		v.(*streamSupervisor).ctrl <- msgDeviceDisconnect{}
		return
	}
	// 无 supervisor 时兜底（例如仅创建了 streamer 尚未走 supervisor）
	if s, ok := m.deviceStreamers.LoadAndDelete(deviceUDID); ok {
		s.(*AndroidStreamer).Stop()
		logutil.Debugf("[WebRTC] 设备 %s: scrcpy-server已停止", deviceUDID)
	}
	m.closeAllSessionsPeerConns(deviceUDID, false)
	m.streams.Delete(deviceUDID)
}

// removeStreamSession 将断连交给 supervisor：单 goroutine LoadAndDelete 会话并维护 refcount / 是否停 scrcpy
func (m *WebRTCManager) removeStreamSession(deviceUDID, sessionID string) {
	m.supervisorFor(deviceUDID).postPeerClose(sessionID)
}

// DisconnectWebRTC 手动断开：sessionID 非空时仅移除该 WebRTC 会话；空则整设备 teardown（等同设备拔线/批量断开）。
func (m *WebRTCManager) DisconnectWebRTC(deviceUDID, sessionID string) {
	sid := strings.TrimSpace(sessionID)
	if sid != "" {
		logutil.Debugf("[WebRTC] 设备 %s: 手动断开单会话 %s", deviceUDID, sid)
		m.removeStreamSession(deviceUDID, sid)
		return
	}
	logutil.Debugf("[WebRTC] 设备 %s: 收到整设备手动断开请求", deviceUDID)
	m.handleDeviceDisconnect(deviceUDID)
}

// HandleManualDisconnect 处理手动断开连接（整设备，兼容旧调用）
func (m *WebRTCManager) HandleManualDisconnect(deviceUDID string) {
	m.DisconnectWebRTC(deviceUDID, "")
}

// RemoveStream 移除设备的所有会话
func (m *WebRTCManager) RemoveStream(deviceUDID string) {
	m.closeAllSessionsPeerConns(deviceUDID, false)
	m.handleDeviceDisconnect(deviceUDID)
}

// SetUnifiedDeviceManager 设置统一设备管理器（仅启动时调用）
func (m *WebRTCManager) SetUnifiedDeviceManager(udm *devicemgr.UnifiedDeviceManager) {
	m.unifiedDeviceManager = udm
}

// offerBuildResult Offer 工作协程与主 goroutine 之间的结果；不含 scrcpy 挂轨（另起 goroutine）。
type offerBuildResult struct {
	err           error
	session       *StreamSession
	answer        webrtc.SessionDescription
	iceCandidates []webrtc.ICECandidateInit
	attachCh      chan error // 首连非 nil：后台 <-attachCh 后挂轨
}

// finishScrcpyAttach 在独立 goroutine 中 <-chan 与 scrcpy Start() 汇合后挂 WebRTC 轨；会话已关闭则放弃。
func (m *WebRTCManager) finishScrcpyAttach(deviceUDID, sessionID string, ch <-chan error) {
	e := <-ch
	if e != nil {
		logutil.Errorf("[WebRTC] 设备 %s: scrcpy 启动失败（后台挂轨）: %v", deviceUDID, e)
		m.removeStreamSession(deviceUDID, sessionID)
		return
	}
	sess, ok := m.GetStreamBySessionID(deviceUDID, sessionID)
	if !ok {
		return
	}
	v, ok := m.deviceStreamers.Load(deviceUDID)
	if !ok {
		m.removeStreamSession(deviceUDID, sessionID)
		return
	}
	st := v.(*AndroidStreamer)
	sess.streamer = st
	if err := m.supervisorFor(deviceUDID).registerVideoPeer(st, sess.sessionID, sess.VideoTrack, sess.PeerConn); err != nil {
		logutil.Errorf("[WebRTC] 设备 %s: 后台挂接视频轨失败: %v", deviceUDID, err)
		m.removeStreamSession(deviceUDID, sessionID)
		return
	}
	if sess.AudioTrack != nil {
		sess.streamer.AddAudioTrack(sess.sessionID, sess.AudioTrack)
	}
	logutil.Debugf("[WebRTC] 设备 %s: scrcpy 已就绪，WebRTC 轨已挂到广播链（后台）", deviceUDID)
}

// HandleWebRTCOfferResolved 处理 WebRTC Offer（由 API 传入已解析的 endpointID+deviceKey，多端点时 stream 以 deviceKey@endpointID 为 key）
func (m *WebRTCManager) HandleWebRTCOfferResolved(c *gin.Context, endpointID, deviceKey string) {
	deviceUDID := deviceKey
	if endpointID != "" {
		deviceUDID = deviceKey + "@" + endpointID
	}
	logutil.Debugf("[WebRTC] 收到设备 %s 的WebRTC Offer请求", deviceUDID)

	var req struct {
		Offer      webrtc.SessionDescription `json:"offer"`
		ICEServers *[]webrtc.ICEServer       `json:"ice_servers,omitempty"`
		MaxSize    *int                      `json:"max_size,omitempty"`
		BitRate    *int                      `json:"bit_rate,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		logutil.Errorf("[WebRTC] 设备 %s: 解析Offer失败: %v", deviceUDID, err)
		c.JSON(400, gin.H{"error": "无效的Offer"})
		return
	}

	maxSize, videoBitRate := 0, 0
	if req.MaxSize != nil && *req.MaxSize > 0 {
		maxSize = *req.MaxSize
	}
	if req.BitRate != nil && *req.BitRate > 0 {
		videoBitRate = *req.BitRate
	}

	dev, exists := m.unifiedDeviceManager.GetDeviceInEndpoint(endpointID, deviceKey)
	var activeDeviceManager *devicemgr.Manager
	if exists {
		activeDeviceManager, _ = m.unifiedDeviceManager.GetManagerForEndpoint(endpointID)
	}
	if !exists {
		logutil.Errorf("[WebRTC] 设备 %s: 设备不存在", deviceUDID)
		c.JSON(404, gin.H{"error": "设备不存在"})
		return
	}

	if activeDeviceManager == nil {
		logutil.Errorf("[WebRTC] 设备 %s: 设备管理器未初始化", deviceUDID)
		c.JSON(500, gin.H{"error": "设备管理器未初始化"})
		return
	}

	logutil.Debugf("[WebRTC] 设备 %s: 开始创建WebRTC会话...", deviceUDID)

	var customICEServers []webrtc.ICEServer
	if req.ICEServers != nil && len(*req.ICEServers) > 0 {
		customICEServers = *req.ICEServers
		logutil.Debugf("[WebRTC] 设备 %s: 使用传入的自定义ICE服务器 (%d 个)", deviceUDID, len(customICEServers))
	}

	offerDone := make(chan offerBuildResult, 1)
	go func() {
		res := offerBuildResult{}
		defer func() {
			if r := recover(); r != nil {
				if res.err == nil {
					res.err = fmt.Errorf("offer 处理 panic: %v", r)
				}
				logutil.Errorf("[WebRTC] 设备 %s: %v", deviceUDID, res.err)
			}
			offerDone <- res
		}()

		logutil.Debugf("[WebRTC] 设备 %s: [协程] CreateStream / SDP / SetLocal...", deviceUDID)
		session, err := m.CreateStream(dev, customICEServers, maxSize, videoBitRate, deviceUDID)
		if err != nil {
			res.err = err
			return
		}
		res.session = session
		sid := session.sessionID

		logutil.Debugf("[WebRTC] 设备 %s: [协程] SetRemoteDescription...", deviceUDID)
		if err := session.PeerConn.SetRemoteDescription(req.Offer); err != nil {
			m.removeStreamSession(deviceUDID, sid)
			res.err = fmt.Errorf("设置远程描述失败: %v", err)
			return
		}
		m.applyPendingICECandidates(deviceUDID, sid, session)

		logutil.Debugf("[WebRTC] 设备 %s: [协程] CreateAnswer...", deviceUDID)
		answer, err := session.PeerConn.CreateAnswer(nil)
		if err != nil {
			m.removeStreamSession(deviceUDID, sid)
			res.err = fmt.Errorf("创建Answer失败: %v", err)
			return
		}

		logutil.Debugf("[WebRTC] 设备 %s: Answer创建完成，DataChannel ID: %d, Label: %s",
			deviceUDID, session.DataChannel.ID(), session.DataChannel.Label())
		if answer.SDP != "" {
			if strings.Contains(answer.SDP, "m=application") {
				logutil.Debugf("[WebRTC] 设备 %s: ✓ Answer SDP 含 m=application", deviceUDID)
			} else {
				logutil.Debugf("[WebRTC] 设备 %s: ⚠️ Answer SDP 未找到 m=application（部分客户端仍可用）", deviceUDID)
			}
		}

		// 不等 scrcpy：先 SetLocal，Answer 即可返回；挂轨走 attachCh 后台 goroutine
		logutil.Debugf("[WebRTC] 设备 %s: [协程] SetLocalDescription（不等待 scrcpy）...", deviceUDID)
		if err := session.PeerConn.SetLocalDescription(answer); err != nil {
			m.removeStreamSession(deviceUDID, sid)
			res.err = fmt.Errorf("设置本地描述失败: %v", err)
			return
		}
		if session.DataChannel != nil {
			logutil.Debugf("[WebRTC] 设备 %s: SetLocal 后 DataChannel: %s",
				deviceUDID, session.DataChannel.ReadyState().String())
		}

		time.Sleep(50 * time.Millisecond)
		session.mu.Lock()
		res.iceCandidates = make([]webrtc.ICECandidateInit, len(session.iceCandidates))
		copy(res.iceCandidates, session.iceCandidates)
		session.mu.Unlock()

		res.answer = answer
		if ch := session.scrcpyConnectDone; ch != nil {
			session.scrcpyConnectDone = nil
			res.attachCh = ch
		}
	}()

	res := <-offerDone
	if res.err != nil {
		logutil.Debugf("[WebRTC] 设备 %s: Offer 工作协程失败: %v", deviceUDID, res.err)
		c.JSON(500, gin.H{"error": res.err.Error()})
		return
	}
	session := res.session

	if res.attachCh != nil {
		go m.finishScrcpyAttach(deviceUDID, session.sessionID, res.attachCh)
	} else {
		if session.streamer == nil {
			logutil.Errorf("[WebRTC] 设备 %s: streamer 为 nil", deviceUDID)
			m.removeStreamSession(deviceUDID, session.sessionID)
			c.JSON(500, gin.H{"error": "streamer未初始化"})
			return
		}
		if err := session.streamer.StartReading(); err != nil {
			logutil.Errorf("[WebRTC] 设备 %s: StartReading 失败: %v", deviceUDID, err)
			m.removeStreamSession(deviceUDID, session.sessionID)
			c.JSON(500, gin.H{"error": fmt.Sprintf("开始读取视频帧失败: %v", err)})
			return
		}
		logutil.Debugf("[WebRTC] 设备 %s: 视频帧读取已启动", deviceUDID)
	}

	logutil.Debugf("[WebRTC] 设备 %s: 返回 Answer 与 %d 个 ICE 候选", deviceUDID, len(res.iceCandidates))
	c.JSON(200, gin.H{
		"answer":        res.answer,
		"iceCandidates": res.iceCandidates,
		"sessionId":     session.sessionID,
		"ready":         true,
	})
}

// HandleICE 处理ICE候选，deviceUDID 由 API 层从解析结果生成（与 Offer 存流时使用的 key 一致）
func (m *WebRTCManager) HandleICE(c *gin.Context, deviceUDID string) {
	var req struct {
		Candidate webrtc.ICECandidateInit `json:"candidate"`
		SessionID string                  `json:"sessionId,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "无效的ICE候选"})
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		c.JSON(400, gin.H{"error": "缺少sessionId"})
		return
	}

	session, exists := m.GetStreamBySessionID(deviceUDID, req.SessionID)

	if !exists || session.PeerConn.RemoteDescription() == nil {
		// 会话未创建或 Offer 尚在独立协程中 SetRemote，暂存候选，SetRemote 后会应用
		m.appendPendingICECandidate(deviceUDID, strings.TrimSpace(req.SessionID), req.Candidate)
		c.JSON(200, gin.H{"message": "ICE候选已暂存（稍后应用）"})
		return
	}

	if err := session.PeerConn.AddICECandidate(req.Candidate); err != nil {
		logutil.Errorf("[WebRTC] 设备 %s: 添加ICE候选失败: %v", deviceUDID, err)
		c.JSON(500, gin.H{"error": fmt.Sprintf("添加ICE候选失败: %v", err)})
		return
	}

	logutil.Debugf("[WebRTC] 设备 %s: ICE候选已添加", deviceUDID)
	c.JSON(200, gin.H{"message": "ICE候选已添加"})
}

func pendingICECandidatesKey(streamKey, sessionID string) string {
	return streamKey + "\x00" + strings.TrimSpace(sessionID)
}

// appendPendingICECandidate SetRemote 前暂存；一路一连一桶。若已 SetRemote 则直接 AddICECandidate。
func (m *WebRTCManager) appendPendingICECandidate(streamKey, sessionID string, c webrtc.ICECandidateInit) {
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return
	}
	if sess, ok := m.GetStreamBySessionID(streamKey, sid); ok && sess.PeerConn != nil && sess.PeerConn.RemoteDescription() != nil {
		_ = sess.PeerConn.AddICECandidate(c)
		return
	}
	key := pendingICECandidatesKey(streamKey, sid)
	val, _ := m.pendingICECandidates.LoadOrStore(key, newPendingICEList())
	val.(*pendingICEList).appendIce(c)
}

// applyPendingICECandidates LoadAndDelete 整桶后在桶内锁下取出切片。
func (m *WebRTCManager) applyPendingICECandidates(streamKey, sessionID string, session *StreamSession) {
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return
	}
	key := pendingICECandidatesKey(streamKey, sid)
	val, loaded := m.pendingICECandidates.LoadAndDelete(key)
	if !loaded {
		return
	}
	list := val.(*pendingICEList).take()
	for _, c := range list {
		_ = session.PeerConn.AddICECandidate(c)
	}
	if len(list) > 0 {
		logutil.Debugf("[WebRTC] 设备 %s 会话 %s: 已应用 %d 个先到的 ICE 候选", streamKey, sid, len(list))
	}
}

// handleControlMessage 将 DataChannel 控制面交给 streamSupervisor，与 scrcpy 写出同序串行（心跳不入队）
func (m *WebRTCManager) handleControlMessage(deviceUDID, sessionID string, data []byte) {
	if len(data) == 0 {
		return
	}
	if data[0] == 0xFF {
		return
	}
	p := make([]byte, len(data))
	copy(p, data)
	m.supervisorFor(deviceUDID).postWebRTCControl(sessionID, p)
}
