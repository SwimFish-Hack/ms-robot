package streaming

import (
	"bufio"
	"crypto/rand"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ms-robots/ms-robot/internal/apk"
	"github.com/ms-robots/ms-robot/internal/deviceupload"
	"github.com/ms-robots/ms-robot/internal/logutil"
	adb "github.com/ms-robots/ms-robot/third_party/adb"
)

// 与设备端 app_process 启动的 com.genymobile.scrcpy.Server 进程匹配（pkill -f）
const scrcpyServerJavaPkillPattern = "com.genymobile.scrcpy.Server"

// 对齐 app/src/controller.c:
// SC_CONTROL_MSG_QUEUE_LIMIT=60，且为不可丢弃消息额外预留 4 个槽位。
const (
	scControlMsgQueueLimit = 60
	scControlSendQueueCap  = scControlMsgQueueLimit + 4
)

// deviceMsgMaxSize 对齐 app/src/device_msg.h DEVICE_MSG_MAX_SIZE（控制流上设备→主机消息上限）
const deviceMsgMaxSize = 1 << 18

// scrcpyDeviceMessageFrameAtStart 对齐 app/src/device_msg.c sc_device_msg_deserialize。
// 返回值：frameLen 为本条消息占用字节数；ok=false 表示未知 type（对应 C 返回 -1）；frameLen==0 且 ok=true 表示数据不足一条完整消息（对应 C 返回 0）。
func scrcpyDeviceMessageFrameAtStart(buf []byte) (frameLen int, ok bool) {
	if len(buf) == 0 {
		return 0, true
	}
	switch buf[0] {
	case 0: // DEVICE_MSG_TYPE_CLIPBOARD
		if len(buf) < 5 {
			return 0, true
		}
		clipboardLen := binary.BigEndian.Uint32(buf[1:5])
		if uint64(clipboardLen) > uint64(len(buf)-5) {
			return 0, true
		}
		return 5 + int(clipboardLen), true
	case 1: // DEVICE_MSG_TYPE_ACK_CLIPBOARD
		if len(buf) < 9 {
			return 0, true
		}
		return 9, true
	case 2: // DEVICE_MSG_TYPE_UHID_OUTPUT
		if len(buf) < 5 {
			return 0, true
		}
		sz := int(binary.BigEndian.Uint16(buf[3:5]))
		if len(buf) < 5+sz {
			return 0, true
		}
		return 5 + sz, true
	default:
		return 0, false
	}
}

// newScrcpySCID 生成官方协议中的 31-bit scid；socket 名为 scrcpy_%08x。
func newScrcpySCID() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err == nil {
		v := binary.BigEndian.Uint32(b[:]) & 0x7fffffff
		if v != 0 {
			return v
		}
	}
	return uint32(time.Now().UnixNano()) & 0x7fffffff
}

func isDroppableControlMessageType(t byte) bool {
	// 对齐 control_msg.c sc_control_msg_is_droppable():
	// 仅 UHID_CREATE(0x0c) / UHID_DESTROY(0x0e) 不可丢。
	return t != 0x0c && t != 0x0e
}

// dialTCPFunc 连接 adb 主机上某地址；nil 表示直连 net.Dial
type dialTCPFunc func(addr string) (net.Conn, error)

// AudioSink 接收设备端 Opus 音频并转发（如写到 WebRTC 轨）
type AudioSink interface {
	WriteAudio(data []byte, duration time.Duration)
}

// ScrcpyServer scrcpy-server管理器
type ScrcpyServer struct {
	adbDevice          *adb.Device // 与 Manager 复用的设备句柄（RunCommand/upload 等）
	adbClient          *adb.Adb    // 与 Manager 复用的 adb 客户端（Forward/OpenCommand）
	adbHost            string      // ADB 所在主机（连接 forward 端口用，adb 在远端时用此 host）
	dialTCP            dialTCPFunc // 可选：proxy 端点时经代理连接 forward 端口
	deviceUDID         string
	assetsFS           embed.FS
	remotePath         string
	maxSize            int
	videoBitRate       int
	scrcpyShellConn    io.Closer // 常驻 adb shell 连接，不断则 scrcpy-server 常驻（不依赖 daemon）
	videoSocket        io.ReadCloser
	controlConn        net.Conn
	forwardPort        int // 映射中的本机端口；与官方一致在 TCP 就绪后即 ForwardRemove 置 0，Stop 时仅在为非 0 时再删（双保险）
	running            bool
	controlOnly        bool // 仅控制、不开视频/音频流
	enableVideo        bool // 是否采集视频（与 controlOnly 互斥，投屏时 true）
	enableAudio        bool // 是否采集音频，与视频独立、可选开关
	stopChan           chan struct{}
	startAbortChan     chan struct{}     // 批量断开时通知正在执行的 Start() 中止
	onDeviceMessage    func([]byte)      // 设备消息回调函数
	onDeviceDisconnect func()            // 设备断开连接回调函数
	onDeviceEnded      func(string)      // 控制链路结束原因（对齐 scrcpy.c: device_disconnected/controller_error）
	uploadMD5Cache     map[string]string // embedPath -> MD5，供 deviceupload.Cache
	mu                 sync.Mutex

	// 控制消息发送：对齐官方 scrcpy controller.c（单写线程 + 队列）。不得多 goroutine 直接 Write(controlConn)。
	controlOut chan []byte
	controlWg  sync.WaitGroup
	// 控制读协程按需启动：避免池化常驻阶段过早进入 receiver 读循环。
	controlReaderStarted bool

	// 音频：设备端为 Opus，协议为 4 字节 codec id + (8 字节 PTS/flags + 4 字节 size + payload)*，同一连接只发一次头
	audioConn       net.Conn
	audioSink       AudioSink
	audioReaderOnce sync.Once

	// androidSDK 来自 ro.build.version.sdk；-1 表示未读取（如音频 <30 禁用等策略）。
	androidSDK int
	scid       uint32
}

const (
	scrcpyEndedDeviceDisconnected = "device_disconnected"
	scrcpyEndedControllerError    = "controller_error"
)

// SetAudioSink 设置音频接收端；读协程已随连接启动，设后即会转发到 sink
func (s *ScrcpyServer) SetAudioSink(sink AudioSink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audioSink = sink
}

// NewScrcpyServer 创建 scrcpy-server 管理器。adbDevice 为 *adb.Device，adbClient 为与 Manager 复用的 adb 客户端，adbHost 为 ADB 所在主机（连接 forward 端口用）。dialTCP 可选，proxy 端点时经代理连接。
func NewScrcpyServer(adbDevice *adb.Device, adbClient *adb.Adb, deviceUDID string, assetsFS embed.FS, adbHost string, dialTCP dialTCPFunc) (*ScrcpyServer, error) {
	if adbHost == "" {
		adbHost = "127.0.0.1"
	}
	return &ScrcpyServer{
		adbDevice:          adbDevice,
		adbClient:          adbClient,
		adbHost:            adbHost,
		dialTCP:            dialTCP,
		deviceUDID:         deviceUDID,
		assetsFS:           assetsFS,
		remotePath:         apk.ScrcpyServerRemotePath,
		maxSize:            0,     // 0表示不限制
		enableVideo:        true,  // 投屏时开视频
		enableAudio:        false, // 默认不采集音频，与视频分开、可选
		stopChan:           make(chan struct{}),
		startAbortChan:     make(chan struct{}, 1),
		onDeviceDisconnect: nil, // 由 AndroidStreamer 设置
		uploadMD5Cache:     make(map[string]string),
		androidSDK:         -1,
	}, nil
}

// SetOnDeviceDisconnect 设置设备断开连接回调函数
func (s *ScrcpyServer) SetOnDeviceDisconnect(callback func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onDeviceDisconnect = callback
}

// SetOnDeviceEnded 设置控制链路结束原因回调（对齐 scrcpy.c 控制事件分型）。
func (s *ScrcpyServer) SetOnDeviceEnded(callback func(reason string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onDeviceEnded = callback
}

func (s *ScrcpyServer) notifyDeviceEnded(reason string) {
	s.mu.Lock()
	cb := s.onDeviceDisconnect
	cbReason := s.onDeviceEnded
	s.mu.Unlock()
	if cbReason != nil {
		cbReason(reason)
	}
	if cb != nil {
		cb()
	}
}

// SetMaxSize 设置最大长边（像素），0表示不限制、启动命令中不拼接该参数
func (s *ScrcpyServer) SetMaxSize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxSize = n
}

// SetVideoBitRate 设置视频比特率（bps），0表示不拼接该参数
func (s *ScrcpyServer) SetVideoBitRate(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.videoBitRate = n
}

// SetControlOnly 仅控制模式：拉起 scrcpy 但不开视频/音频流，只建立控制连接
func (s *ScrcpyServer) SetControlOnly(controlOnly bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.controlOnly = controlOnly
	s.enableVideo = !controlOnly
}

// SetEnableAudio 设置是否采集音频（与视频独立；需在 Start 前调用）
func (s *ScrcpyServer) SetEnableAudio(enable bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enableAudio = enable
}

// EnableAudio 返回当前是否采集音频
func (s *ScrcpyServer) EnableAudio() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enableAudio
}

// ControlOnly 返回当前是否仅控制（无视频/音频流）
func (s *ScrcpyServer) ControlOnly() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.controlOnly
}

// getScrcpyDelay 读取延时（ms）环境变量，ADB 延迟高时可调大。key 如 SCRCPY_STOP_WAIT_MS，defaultMs 为默认值。
func getScrcpyDelay(key string, defaultMs int) time.Duration {
	if s := os.Getenv(key); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return time.Duration(defaultMs) * time.Millisecond
}

func getScrcpyIntEnv(key string, defaultVal int) int {
	if s := os.Getenv(key); s != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
			return n
		}
	}
	return defaultVal
}

// connectForwardReadDummy 对齐 server.c connect_to_server + connect_and_read_byte：
// 每次新建 TCP、读 1 字节 dummy；Dial 或读失败则关闭连接、sleep 后重试。老设备 app_process/JVM 慢时首连易 EOF，需多试几次。
func (s *ScrcpyServer) connectForwardReadDummy(connectAddr string, maxAttempts int, retryDelay time.Duration, readDeadline time.Duration) (net.Conn, error) {
	var lastErr error
	var conn net.Conn
	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-s.startAbortChan:
			if conn != nil {
				_ = conn.Close()
			}
			return nil, fmt.Errorf("已停止")
		default:
		}
		if conn != nil {
			_ = conn.Close()
			conn = nil
		}
		c, dialErr := s.dialToForward(connectAddr, 3*time.Second)
		if dialErr != nil {
			lastErr = dialErr
			logutil.Debugf("[scrcpy-server] 设备 %s: forward Dial 重试 %d/%d: %v", s.deviceUDID, attempt+1, maxAttempts, dialErr)
			if attempt+1 < maxAttempts {
				time.Sleep(retryDelay)
			}
			continue
		}
		conn = c
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			tcpConn.SetKeepAlive(true)
			tcpConn.SetKeepAlivePeriod(30 * time.Second)
			tcpConn.SetNoDelay(true)
		}
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			tcpConn.SetReadDeadline(time.Now().Add(readDeadline))
		}
		dummy := make([]byte, 1)
		_, readErr := io.ReadFull(conn, dummy)
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			tcpConn.SetReadDeadline(time.Time{})
		}
		if readErr == nil {
			return conn, nil
		}
		lastErr = readErr
		logutil.Debugf("[scrcpy-server] 设备 %s: tunnel_forward dummy 重试 %d/%d: %v", s.deviceUDID, attempt+1, maxAttempts, readErr)
		if attempt+1 < maxAttempts {
			time.Sleep(retryDelay)
		}
	}
	if conn != nil {
		_ = conn.Close()
	}
	return nil, lastErr
}

// getScrcpyServerClientVersion 传给 com.genymobile.scrcpy.Server 的首参，必须与 jar 内 BuildConfig.VERSION_NAME 完全一致（见 apk.ScrcpyServerClientVersion）。
func getScrcpyServerClientVersion() string {
	if v := strings.TrimSpace(os.Getenv("SCRCPY_SERVER_VERSION")); v != "" {
		return v
	}
	return apk.ScrcpyServerClientVersion
}

func (s *ScrcpyServer) pkillScrcpyJavaServer() {
	if s.adbDevice == nil {
		return
	}
	_, _ = s.adbDevice.RunCommand("pkill", "-f", scrcpyServerJavaPkillPattern)
}

// applyOfficialControlSocketOptions 对齐 server.c：控制 socket 上 SetTCPNoDelay（及 KeepAlive）。
func (s *ScrcpyServer) applyOfficialControlSocketOptions(c net.Conn) {
	if tcpConn, ok := c.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
		tcpConn.SetNoDelay(true)
	}
}

// forwardRemoveAfterTunnelConnect 在视频/控制 TCP 均已就绪后调用，对齐官方 sc_adb_tunnel_close。
func (s *ScrcpyServer) forwardRemoveAfterTunnelConnect(port int) {
	s.forwardRemoveByPortIfActive(port)
	logutil.Debugf("[scrcpy-server] 设备 %s: adb forward 已拆除（对齐 sc_adb_tunnel_close），原 tcp:%d", s.deviceUDID, port)
}

// forwardRemoveByPortIfActive 若当前仍持有该 port 的 adb forward 则删除并清零 forwardPort。
// 用于失败清理；亦在 TCP 全部就绪后由 forwardRemoveAfterTunnelConnect 调用，对齐 server.c sc_adb_forward_remove（sc_adb_tunnel_close）。
func (s *ScrcpyServer) forwardRemoveByPortIfActive(port int) {
	if port == 0 || s.adbDevice == nil {
		return
	}
	s.mu.Lock()
	if s.forwardPort != port {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	_ = s.adbDevice.ForwardRemove(adb.ForwardSpec{Protocol: adb.FProtocolTcp, PortOrName: strconv.Itoa(port)})
	s.mu.Lock()
	if s.forwardPort == port {
		s.forwardPort = 0
	}
	s.mu.Unlock()
}

// startControlWriter 启动唯一控制写协程（对齐 controller.c run_controller + 队列）。
func (s *ScrcpyServer) startControlWriter() {
	s.mu.Lock()
	if s.controlOut != nil {
		s.mu.Unlock()
		return
	}
	s.controlOut = make(chan []byte, scControlSendQueueCap)
	s.mu.Unlock()
	s.controlWg.Add(1)
	go s.controlWriterLoop()
}

// ensureControlLoopsStarted 启动控制读/写协程（幂等）。对齐官方 server.c + scrcpy.c：须在视频 socket 上读完 device_read_info（64B）且 s.running=true 之后再在控制 socket 上 recv（先 TCP 建全 → tunnel_close → 读 64B → 再 sc_controller_start）。
func (s *ScrcpyServer) ensureControlLoopsStarted() {
	s.startControlWriter()
	s.mu.Lock()
	if s.controlConn == nil || s.controlReaderStarted || !s.running {
		s.mu.Unlock()
		return
	}
	conn := s.controlConn
	s.controlReaderStarted = true
	s.mu.Unlock()
	go s.handleControlStream(conn)
}

func (s *ScrcpyServer) controlWriterLoop() {
	defer s.controlWg.Done()
	for data := range s.controlOut {
		s.mu.Lock()
		conn := s.controlConn
		running := s.running
		s.mu.Unlock()
		if !running || conn == nil {
			continue
		}
		n, err := conn.Write(data)
		if err != nil {
			// 对齐 controller.c：process_msg() 写失败即退出 controller，随后上抛为会话断开/错误。
			logutil.Errorf("[scrcpy-server] ❌ 设备 %s 控制写失败: %v", s.deviceUDID, err)
			s.mu.Lock()
			isCurrentConn := s.controlConn == conn
			if isCurrentConn {
				s.controlConn = nil
				s.controlReaderStarted = false
			}
			wasRunning := isCurrentConn && s.running
			if isCurrentConn && s.running {
				s.running = false
			}
			s.mu.Unlock()
			_ = conn.Close()
			if !isCurrentConn {
				logutil.Debugf("[scrcpy-server] 设备 %s: 忽略过期控制连接写失败（当前连接已切换）", s.deviceUDID)
				continue
			}
			if wasRunning {
				logutil.Infof("[scrcpy-server] 设备 %s: 控制写失败，停止服务", s.deviceUDID)
				s.notifyDeviceEnded(scrcpyEndedDeviceDisconnected)
			}
			return
		}
		if n != len(data) {
			logutil.Debugf("[scrcpy-server] ⚠️ 设备 %s 控制写不完整: %d/%d", s.deviceUDID, n, len(data))
		}
	}
}

// Start 启动scrcpy-server并连接
// 自动处理：推送文件、建立forward、启动server（listen模式）、连接forward端口
func (s *ScrcpyServer) Start() error {
	// 清掉上次可能残留的中止信号，避免本次 Start 被误判为中止
	select {
	case <-s.startAbortChan:
	default:
	}
	s.mu.Lock()
	// 先停止之前的进程（如果存在），确保完全清理
	if s.running {
		logutil.Debugf("检测到之前的server进程，先停止...")
		s.stopUnlocked()
		d := getScrcpyDelay("SCRCPY_STOP_WAIT_MS", 500)
		logutil.Infof("[scrcpy-server] 延时 %dms (SCRCPY_STOP_WAIT_MS)，等进程退出...", d.Milliseconds())
		time.Sleep(d)
	}
	s.mu.Unlock()

	// 1. 推送jar文件到手机
	logutil.Debugf("[scrcpy-server] 设备 %s: 步骤1 推送/检查 server...", s.deviceUDID)
	if err := s.pushServer(); err != nil {
		logutil.Errorf("[scrcpy-server] 设备 %s: Start 失败 at 推送server: %v", s.deviceUDID, err)
		return fmt.Errorf("推送server失败: %v", err)
	}

	// 2. 设置执行权限
	logutil.Debugf("[scrcpy-server] 设备 %s: 步骤2 设置权限...", s.deviceUDID)
	if err := s.setPermissions(); err != nil {
		logutil.Errorf("[scrcpy-server] 设备 %s: Start 失败 at 设置权限: %v", s.deviceUDID, err)
		return fmt.Errorf("设置权限失败: %v", err)
	}

	// 3. 建立forward并启动server（设备listen，主机connect）；running 在 startServerAndConnect 内控制连接就绪后设置
	logutil.Debugf("[scrcpy-server] 设备 %s: 步骤3 startServerAndConnect...", s.deviceUDID)
	if err := s.startServerAndConnect(); err != nil {
		logutil.Errorf("[scrcpy-server] 设备 %s: Start 失败 at startServerAndConnect: %v", s.deviceUDID, err)
		return fmt.Errorf("启动server失败: %v", err)
	}

	logutil.Debugf("scrcpy-server已启动并连接: %s", s.deviceUDID)
	return nil
}

// startServerAndConnect 启动 server 并建立连接
// 使用 adb forward：设备端 server 在 localabstract:scrcpy 上 listen，主机连接 adb 所在机的 forward 端口（adb 在远端时也可用）。
func (s *ScrcpyServer) startServerAndConnect() error {
	if s.adbDevice == nil {
		return fmt.Errorf("ADB 不可用")
	}
	// 1. 仅使用 host forward（tunnel_forward=true），与 scrcpy --force-adb-forward 一致；不使用 adb reverse。
	androidSDK := -1
	if sdkRaw, sdkErr := s.adbDevice.RunCommand("getprop", "ro.build.version.sdk"); sdkErr == nil {
		if sdk, convErr := strconv.Atoi(strings.TrimSpace(sdkRaw)); convErr == nil {
			androidSDK = sdk
		}
	}
	s.mu.Lock()
	s.androidSDK = androidSDK
	s.mu.Unlock()

	scid := newScrcpySCID()
	socketName := fmt.Sprintf("scrcpy_%08x", scid)

	// 2. 先启动设备端 scrcpy-server（在设备上 listen localabstract）
	s.mu.Lock()
	maxSize, videoBitRate, controlOnly := s.maxSize, s.videoBitRate, s.controlOnly
	s.mu.Unlock()
	clientVer := getScrcpyServerClientVersion()
	serverArgs := []string{
		"CLASSPATH=" + s.remotePath,
		"app_process", "/",
		"com.genymobile.scrcpy.Server",
		clientVer,
		fmt.Sprintf("scid=%08x", scid),
	}
	// GET_CLIPBOARD 直连读设备剪贴板须 clipboard_autosync=false；true 时同官方仅监听变更。默认 false；SCRCPY_CLIPBOARD_AUTOSYNC=true 可改。
	cbAuto := false
	if s := strings.TrimSpace(os.Getenv("SCRCPY_CLIPBOARD_AUTOSYNC")); s != "" {
		if b, err := strconv.ParseBool(s); err == nil {
			cbAuto = b
		}
	}
	serverArgs = append(serverArgs, "log_level=debug", "tunnel_forward=true", fmt.Sprintf("clipboard_autosync=%t", cbAuto))
	s.mu.Lock()
	enableVideo, enableAudio := s.enableVideo, s.enableAudio
	s.mu.Unlock()

	s.mu.Lock()
	s.scid = scid
	s.mu.Unlock()
	deviceSpec := adb.ForwardSpec{Protocol: adb.FProtocolAbstract, PortOrName: socketName}

	// Android 11 以下不支持 scrcpy 音频采集，强制关闭避免设备端反复报错刷屏。
	// 须同时改此局部变量并用于下方 TCP 拨号；若只写 argv 的 audio=false 仍按 s.enableAudio 拨「音频+控制」两路，
	// 会与 DesktopConnection 的 accept 顺序错位（第二路应是控制，不是音频）。
	if enableAudio && androidSDK >= 0 && androidSDK < 30 {
		logutil.Warnf("[scrcpy-server] 设备 %s Android SDK=%d (<30)，强制关闭音频采集（argv 与 TCP 连接顺序均按无音频）", s.deviceUDID, androidSDK)
		enableAudio = false
		s.mu.Lock()
		s.enableAudio = false
		s.mu.Unlock()
	}
	if controlOnly {
		serverArgs = append(serverArgs, "video=false", "audio=false")
	} else {
		// 官方默认 video=true，仅在需要关闭时传参。
		if !enableVideo {
			serverArgs = append(serverArgs, "video=false")
		}
		// 官方默认 audio=true；当音频被禁用时明确传 false。
		if !enableAudio {
			serverArgs = append(serverArgs, "audio=false")
		}
		if enableVideo {
			if maxSize > 0 {
				serverArgs = append(serverArgs, fmt.Sprintf("max_size=%d", maxSize))
			}
			if videoBitRate > 0 {
				serverArgs = append(serverArgs, fmt.Sprintf("video_bit_rate=%d", videoBitRate))
			}
			// 降低默认帧率以减轻前端解码/渲染压力，减少大量丢帧导致的卡顿。
			// 可通过 SCRCPY_MAX_FPS 覆盖（例如 15、20、30）。
			maxFPS := getScrcpyIntEnv("SCRCPY_MAX_FPS", 20)
			if maxFPS > 0 {
				serverArgs = append(serverArgs, fmt.Sprintf("max_fps=%d", maxFPS))
			}
			serverArgs = append(serverArgs, "video_codec_options=i-frame-interval=1")
		}
		// 采集时设备继续本地出声（ROUTE_FLAG_LOOP_BACK_RENDER），否则采集时设备会静音
		if enableAudio {
			serverArgs = append(serverArgs, "audio_dup=true")
		}
	}
	// 经 adb shell: 协议下发 argv（env + app_process），避免单条 sh -c 拼接整串命令。
	shellConn, err := s.adbDevice.OpenCommand("env", serverArgs...)
	if err != nil {
		logutil.Errorf("[scrcpy-server] 设备 %s: startServerAndConnect 失败 at OpenCommand(env): %v", s.deviceUDID, err)
		return fmt.Errorf("启动 scrcpy-server 失败（OpenCommand）: %v", err)
	}
	go func() {
		out := &scrcpyServerOutput{prefix: fmt.Sprintf("[scrcpy-server][%s] ", s.deviceUDID)}
		scanner := bufio.NewScanner(shellConn)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			_, _ = out.Write([]byte(line + "\n"))
		}
		if scanErr := scanner.Err(); scanErr != nil {
			logutil.Warnf("[scrcpy-server] 设备 %s: 读取 server 输出失败: %v", s.deviceUDID, scanErr)
		}
		_ = shellConn.Close()
	}()
	s.mu.Lock()
	s.scrcpyShellConn = shellConn
	s.mu.Unlock()
	logutil.Debugf("[scrcpy-server] 设备 %s 已通过 adb shell(env+argv) 拉起官方 scrcpy-server", s.deviceUDID)

	// 官方 jar 无 MR_ROBOT 监听标记：短延时后建 forward + dial（首连接 read dummy 与原版一致）
	startupWait := getScrcpyDelay("SCRCPY_STARTUP_WAIT_MS", 400)
	logutil.Debugf("[scrcpy-server] 设备 %s 等待进程初始化 %s (SCRCPY_STARTUP_WAIT_MS)...", s.deviceUDID, startupWait)
	select {
	case <-s.startAbortChan:
		s.pkillScrcpyJavaServer()
		return fmt.Errorf("已停止")
	case <-time.After(startupWait):
	}

	// 3. 建立 adb forward（adb 所在机端口 -> 设备 localabstract:scrcpy），端口在 adb 主机上
	port, err := s.adbDevice.ForwardToFreePort(deviceSpec)
	if err != nil {
		logutil.Errorf("[scrcpy-server] 设备 %s: startServerAndConnect 失败 at ForwardToFreePort: %v", s.deviceUDID, err)
		s.pkillScrcpyJavaServer()
		return fmt.Errorf("建立 ADB forward 失败: %v", err)
	}
	s.mu.Lock()
	s.forwardPort = port
	s.mu.Unlock()
	logutil.Debugf("ADB forward 已建立 tcp:%d -> localabstract:%s (连接 %s:%d)", port, socketName, s.adbHost, port)

	// 4. 主机连接 forward 并读 dummy：对齐 server.c connect_to_server（每次新建连接直到读到 1 字节；老设备 JVM 慢时首连易 EOF）
	connectAddr := net.JoinHostPort(s.adbHost, strconv.Itoa(port))
	dForward := getScrcpyDelay("SCRCPY_FORWARD_WAIT_MS", 200)
	if dForward > 0 {
		time.Sleep(dForward)
	}
	if dRead := getScrcpyDelay("SCRCPY_READ_WAIT_MS", 0); dRead > 0 {
		time.Sleep(dRead)
	}
	dummyMax := getScrcpyIntEnv("SCRCPY_DUMMY_MAX_ATTEMPTS", 100)
	dummyRetry := getScrcpyDelay("SCRCPY_DUMMY_RETRY_MS", 100)

	if controlOnly {
		firstConn, err := s.connectForwardReadDummy(connectAddr, dummyMax, dummyRetry, 10*time.Second)
		if err != nil {
			logutil.Errorf("[scrcpy-server] 设备 %s: startServerAndConnect 失败 at connect+dummy(仅控制): %v", s.deviceUDID, err)
			s.forwardRemoveByPortIfActive(port)
			s.pkillScrcpyJavaServer()
			return fmt.Errorf("读取 tunnel_forward 首字节失败: %v", err)
		}
		logutil.Debugf("scrcpy-server 已连接 (仅控制) via forward %s", connectAddr)
		// 与官方一致：控制 TCP 就绪后先 sc_adb_tunnel_close，再 device_read_info
		s.forwardRemoveAfterTunnelConnect(port)
		if tcpConn, ok := firstConn.(*net.TCPConn); ok {
			tcpConn.SetReadDeadline(time.Now().Add(10 * time.Second))
		}
		deviceInfo := make([]byte, 64)
		if _, err := io.ReadFull(firstConn, deviceInfo); err != nil {
			firstConn.Close()
			s.forwardRemoveByPortIfActive(port)
			s.pkillScrcpyJavaServer()
			return fmt.Errorf("读取设备信息失败: %v", err)
		}
		if tcpConn, ok := firstConn.(*net.TCPConn); ok {
			tcpConn.SetReadDeadline(time.Time{})
		}
		s.mu.Lock()
		s.controlConn = firstConn
		s.mu.Unlock()
		s.applyOfficialControlSocketOptions(firstConn)
		s.mu.Lock()
		s.running = true
		s.mu.Unlock()
		// 与官方一致：控制 TCP 就绪后立即启动 receiver，避免先跑完 WebRTC 再读导致设备端已关控制套接字
		s.ensureControlLoopsStarted()
		logutil.Debugf("已成功连接到 scrcpy-server (仅控制)")
		return nil
	}

	firstConn, err := s.connectForwardReadDummy(connectAddr, dummyMax, dummyRetry, 10*time.Second)
	if err != nil {
		logutil.Errorf("[scrcpy-server] 设备 %s: startServerAndConnect 失败 at connect+dummy: %v", s.deviceUDID, err)
		s.forwardRemoveByPortIfActive(port)
		s.pkillScrcpyJavaServer()
		return fmt.Errorf("读取 tunnel_forward 首字节失败: %v", err)
	}
	videoConn := firstConn
	logutil.Debugf("scrcpy-server 已连接 (视频流) via forward %s", connectAddr)

	// 5. dummy 已读完；与 DesktopConnection.open 一致：首路 accept 后即写 dummy，再阻塞后续 accept — 此时再 dial 音频/控制
	// 必须用上方与 argv 一致的 enableAudio，禁止读 s.enableAudio（低 API 强制关音频后二者会不一致）。

	if enableAudio {
		conn2, err := s.dialToForward(connectAddr, 8*time.Second)
		if err != nil {
			logutil.Errorf("[scrcpy-server] 设备 %s: startServerAndConnect 失败 at Dial(audio,%s): %v", s.deviceUDID, connectAddr, err)
			videoConn.Close()
			s.forwardRemoveByPortIfActive(port)
			s.pkillScrcpyJavaServer()
			return fmt.Errorf("连接音频通道失败: %v", err)
		}
		s.mu.Lock()
		s.audioConn = conn2
		s.mu.Unlock()
		s.startAudioReader(conn2)

		conn3, err := s.dialToForward(connectAddr, 5*time.Second)
		if err != nil {
			logutil.Errorf("[scrcpy-server] 设备 %s: startServerAndConnect 失败 at Dial(control,%s): %v", s.deviceUDID, connectAddr, err)
			videoConn.Close()
			conn2.Close()
			s.forwardRemoveByPortIfActive(port)
			s.pkillScrcpyJavaServer()
			return fmt.Errorf("连接控制通道失败: %v", err)
		}
		s.mu.Lock()
		s.controlConn = conn3
		s.mu.Unlock()
		s.applyOfficialControlSocketOptions(conn3)
	} else {
		conn2, err := s.dialToForward(connectAddr, 8*time.Second)
		if err != nil {
			logutil.Errorf("[scrcpy-server] 设备 %s: startServerAndConnect 失败 at Dial(control,%s): %v", s.deviceUDID, connectAddr, err)
			videoConn.Close()
			s.forwardRemoveByPortIfActive(port)
			s.pkillScrcpyJavaServer()
			return fmt.Errorf("连接控制通道失败: %v", err)
		}
		s.mu.Lock()
		s.controlConn = conn2
		s.mu.Unlock()
		s.applyOfficialControlSocketOptions(conn2)
	}

	// 与官方一致：video/audio/control 的 TCP 均已建立后 sc_adb_tunnel_close，再读 device_read_info（64B）；控制 recv 线程须在 64B 读完之后（对齐 scrcpy.c，在 server_connect_to 返回后才 sc_controller_start）
	s.forwardRemoveAfterTunnelConnect(port)

	// 6. 在 video socket 读 64B 设备名；codec 头由 demuxer/ReadVideoStream
	logutil.Debugf("读取设备信息(64B)；codec 头由首帧 ReadVideoStream 读取（对齐官方 demuxer）...")
	if tcpConn, ok := videoConn.(*net.TCPConn); ok {
		tcpConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	}

	// 读取设备信息（64字节）
	deviceInfo := make([]byte, 64)
	if _, err := io.ReadFull(videoConn, deviceInfo); err != nil {
		logutil.Errorf("[scrcpy-server] 设备 %s: startServerAndConnect 失败 at 读取设备信息(64字节): %v", s.deviceUDID, err)
		videoConn.Close()
		s.pkillScrcpyJavaServer()
		return fmt.Errorf("读取设备信息失败: %v", err)
	}

	// 解析设备名称（UTF-8，以 \0 结尾）
	deviceName := ""
	for i := 0; i < len(deviceInfo); i++ {
		if deviceInfo[i] == 0 {
			deviceName = string(deviceInfo[:i])
			break
		}
	}
	if deviceName == "" {
		deviceName = string(deviceInfo)
	}
	logutil.Debugf("设备信息: %s", deviceName)

	// 对齐 server.c：tunnel_close 后只在视频 socket 读 64B 设备名；12B codec 头由 demuxer 读。
	if tcpConn, ok := videoConn.(*net.TCPConn); ok {
		tcpConn.SetReadDeadline(time.Time{})
	}

	// 8. 创建 ADBSocket 包装连接（视频通道）
	videoSocket := &ADBSocket{
		conn:            videoConn,
		closed:          false,
		deviceInfoRead:  true,  // 已在上面 ReadFull(64)
		videoHeaderRead: false, // 留给 ReadH264Stream（等同 demuxer 读 codec meta）
		configRead:      false,
	}

	s.videoSocket = videoSocket

	logutil.Debugf("已成功连接到 scrcpy-server (视频通道: true)")
	logutil.Debugf("[scrcpy-server] 连接信息: 本地地址=%v, 远程地址=%v", videoConn.LocalAddr(), videoConn.RemoteAddr())

	s.mu.Lock()
	s.running = true
	s.mu.Unlock()
	// 对齐 scrcpy.c：server_connect_to 仅负责 TCP、tunnel_close、读 64B；sc_controller_start 在返回之后由 AndroidStreamer.Start 紧接调用（与 scrcpy.c 分层一致）。仅控制模式见上方 controlOnly 分支内 ensure。
	return nil
}

func (s *ScrcpyServer) dialToForward(addr string, timeout time.Duration) (net.Conn, error) {
	if s.dialTCP != nil {
		return s.dialTCP(addr)
	}
	return net.DialTimeout("tcp", addr, timeout)
}

// startAudioReader 解析设备端音频流（4B codec + 12B 帧头 + payload 循环）并转发到 sink；同连接只发一次头。
func (s *ScrcpyServer) startAudioReader(conn net.Conn) {
	s.audioReaderOnce.Do(func() {
		go func() {
			defer conn.Close()
			buf := make([]byte, 4)
			if _, err := io.ReadFull(conn, buf); err != nil {
				return
			}
			// codec id 已读，设备默认 Opus
			header := make([]byte, 12)
			optsDur := 20 * time.Millisecond
			firstAudioPacket := true
			for {
				select {
				case <-s.stopChan:
					return
				default:
				}
				if _, err := io.ReadFull(conn, header); err != nil {
					return
				}
				ptsAndFlags := uint64(header[0])<<56 | uint64(header[1])<<48 | uint64(header[2])<<40 | uint64(header[3])<<32 |
					uint64(header[4])<<24 | uint64(header[5])<<16 | uint64(header[6])<<8 | uint64(header[7])
				packetSize := uint32(header[8])<<24 | uint32(header[9])<<16 | uint32(header[10])<<8 | uint32(header[11])
				if packetSize == 0 || packetSize > 64*1024 {
					return
				}
				payload := make([]byte, packetSize)
				if _, err := io.ReadFull(conn, payload); err != nil {
					return
				}
				s.mu.Lock()
				sink := s.audioSink
				s.mu.Unlock()
				if sink == nil {
					continue
				}
				if firstAudioPacket {
					logutil.Debugf("[scrcpy-server] 设备 %s [音频] 首次从设备收到音频包 %d 字节", s.deviceUDID, len(payload))
					firstAudioPacket = false
				}
				dur := optsDur
				if (ptsAndFlags & (1 << 63)) != 0 {
					dur = 0
				}
				sink.WriteAudio(payload, dur)
			}
		}()
	})
}

// handleControlStream 处理控制流读侧，对齐 app/src/receiver.c run_receiver + device_msg.c。
// 写侧由 controlWriterLoop 负责（对齐 controller.c）。
func (s *ScrcpyServer) handleControlStream(conn net.Conn) {
	remoteAddr := conn.RemoteAddr()
	localAddr := conn.LocalAddr()
	logutil.Debugf("[scrcpy-server] 设备 %s 开始处理控制流: %v", s.deviceUDID, remoteAddr)

	// 保存控制流连接（只保存最新的）
	s.mu.Lock()
	// 如果已经有控制连接，先关闭旧的（避免资源泄漏）
	if s.controlConn != nil && s.controlConn != conn {
		oldConn := s.controlConn
		logutil.Debugf("[scrcpy-server] 关闭旧的控制连接: %v", oldConn.RemoteAddr())
		oldConn.Close()
	}
	s.controlConn = conn
	s.mu.Unlock()

	// 控制 socket 选项已在 applyOfficialControlSocketOptions 中设置（对齐 server.c）

	deviceMessageChan := make(chan []byte, 10)
	controlStartAt := time.Now()

	teardownControlRead := func(receiverStopped bool, readErr error, readCalls, totalBytes int) {
		if receiverStopped {
			if readErr != nil {
				logutil.Debugf("[scrcpy-server] 设备 %s 控制流 Receiver stopped (%v, readCalls=%d, bytes=%d, alive=%s)",
					s.deviceUDID, readErr, readCalls, totalBytes, time.Since(controlStartAt).Truncate(time.Millisecond))
			} else {
				logutil.Debugf("[scrcpy-server] 设备 %s 控制流 Receiver stopped (readCalls=%d, bytes=%d, alive=%s)",
					s.deviceUDID, readCalls, totalBytes, time.Since(controlStartAt).Truncate(time.Millisecond))
			}
		} else if !receiverStopped {
			logutil.Warnf("[scrcpy-server] 设备 %s 控制流设备消息协议错误（对齐 device_msg 未知 type -> -1）", s.deviceUDID)
		}
		s.mu.Lock()
		isCurrentConn := s.controlConn == conn
		if isCurrentConn {
			s.controlConn = nil
			s.controlReaderStarted = false
		}
		wasRunning := isCurrentConn && s.running
		if isCurrentConn && s.running {
			if receiverStopped {
				logutil.Infof("[scrcpy-server] 设备 %s: 控制流读结束，停止服务（对齐 receiver.c）", s.deviceUDID)
			} else {
				logutil.Infof("[scrcpy-server] 设备 %s: 控制流协议错误，停止服务（对齐 receiver.c process_msgs -1）", s.deviceUDID)
			}
			s.running = false
		}
		s.mu.Unlock()
		_ = conn.Close()
		if !isCurrentConn {
			logutil.Debugf("[scrcpy-server] 设备 %s: 忽略过期控制连接结束（当前连接已切换）", s.deviceUDID)
			return
		}
		if wasRunning {
			if receiverStopped {
				s.notifyDeviceEnded(scrcpyEndedDeviceDisconnected)
			} else {
				s.notifyDeviceEnded(scrcpyEndedControllerError)
			}
		}
	}

	go func() {
		defer close(deviceMessageChan)
		buf := make([]byte, deviceMsgMaxSize)
		head := 0
		readCalls := 0
		totalBytes := 0
		firstReadAt := time.Time{}

		logutil.Debugf("[scrcpy-server] 设备 %s receiver 线程已启动 (local=%v remote=%v)", s.deviceUDID, localAddr, remoteAddr)

		for {
			select {
			case <-s.stopChan:
				logutil.Debugf("[scrcpy-server] 收到停止信号，停止读取设备消息")
				return
			default:
			}

			if head >= deviceMsgMaxSize {
				teardownControlRead(false, nil, readCalls, totalBytes)
				return
			}

			// 对齐 receiver.c：阻塞 net_recv，不设读超时
			readCalls++
			n, err := conn.Read(buf[head:deviceMsgMaxSize])
			if n > 0 {
				totalBytes += n
				head += n
				if firstReadAt.IsZero() {
					firstReadAt = time.Now()
					logutil.Debugf("[scrcpy-server] 设备 %s 控制流首次读到数据: %d bytes (after %s)", s.deviceUDID, n, firstReadAt.Sub(controlStartAt).Truncate(time.Millisecond))
				}
			}

			// 对齐 receiver.c process_msgs + device_msg_deserialize
			consumed := 0
			for {
				flen, ok := scrcpyDeviceMessageFrameAtStart(buf[consumed:head])
				if !ok {
					teardownControlRead(false, nil, readCalls, totalBytes)
					return
				}
				if flen == 0 {
					break
				}
				frame := make([]byte, flen)
				copy(frame, buf[consumed:consumed+flen])
				consumed += flen
				select {
				case deviceMessageChan <- frame:
				case <-s.stopChan:
					return
				}
			}
			if consumed > 0 {
				copy(buf, buf[consumed:head])
				head -= consumed
			}

			if err != nil {
				if errors.Is(err, io.EOF) {
					if head > 0 {
						logutil.Warnf("[scrcpy-server] 设备 %s: 控制流 EOF 时仍有 %d 字节未形成完整设备消息", s.deviceUDID, head)
					}
					teardownControlRead(true, err, readCalls, totalBytes)
					return
				}
				teardownControlRead(true, err, readCalls, totalBytes)
				return
			}
		}
	}()

	// 处理设备消息（转发到回调函数）
	go func() {
		for {
			select {
			case msg, ok := <-deviceMessageChan:
				if !ok {
					return
				}
				if s.onDeviceMessage != nil {
					s.onDeviceMessage(msg)
				}
			case <-s.stopChan:
				return
			}
		}
	}()

	// 等待停止信号
	<-s.stopChan
	logutil.Debugf("[scrcpy-server] 设备 %s 收到停止信号，停止处理控制流 %v", s.deviceUDID, remoteAddr)

	// 清除控制连接引用
	s.mu.Lock()
	if s.controlConn == conn {
		s.controlConn = nil
		s.controlReaderStarted = false
		logutil.Debugf("[scrcpy-server] 设备 %s 已清除控制连接引用", s.deviceUDID)
	}
	s.mu.Unlock()

	// 关闭连接
	conn.Close()
	logutil.Debugf("[scrcpy-server] 设备 %s 控制流连接已关闭: %v", s.deviceUDID, remoteAddr)
}

// SetDeviceMessageHandler 设置设备消息回调函数
func (s *ScrcpyServer) SetDeviceMessageHandler(handler func([]byte)) {
	s.mu.Lock()
	s.onDeviceMessage = handler
	s.mu.Unlock()
	if handler != nil {
		s.ensureControlLoopsStarted()
	}
}

// Get 实现 deviceupload.Cache
func (s *ScrcpyServer) Get(embedPath string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.uploadMD5Cache[embedPath]
}

// Set 实现 deviceupload.Cache
func (s *ScrcpyServer) Set(embedPath, md5 string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uploadMD5Cache == nil {
		s.uploadMD5Cache = make(map[string]string)
	}
	s.uploadMD5Cache[embedPath] = md5
}

// pushServer 将嵌入的官方 scrcpy-server（jar）推送到设备，供 CLASSPATH 启动。
func (s *ScrcpyServer) pushServer() error {
	adapter := &deviceupload.AdbDeviceAdapter{Device: s.adbDevice}
	if err := deviceupload.Upload(s.assetsFS, apk.ScrcpyServerEmbedPath, apk.ScrcpyServerRemotePath, adapter, s, "[scrcpy-server]"); err != nil {
		return fmt.Errorf("上传 scrcpy-server 失败: %w", err)
	}
	s.mu.Lock()
	s.remotePath = apk.ScrcpyServerRemotePath
	s.mu.Unlock()
	return nil
}

// setPermissions 设置执行权限
func (s *ScrcpyServer) setPermissions() error {
	// 设置文件权限
	_, err := s.adbDevice.RunCommand("chmod", "755", s.remotePath)
	return err
}

// ReadVideoStream 读取视频流
// 返回: (帧数据, isConfig, isKeyFrame, error)
func (s *ScrcpyServer) ReadVideoStream() ([]byte, bool, bool, error) {
	if !s.running {
		return nil, false, false, fmt.Errorf("server未运行")
	}

	if s.videoSocket == nil {
		return nil, false, false, fmt.Errorf("视频socket未建立")
	}

	// 从ADB socket读取H.264帧
	// ADBSocket.ReadH264Stream()会解析scrcpy协议并返回H.264帧数据
	if adbSocket, ok := s.videoSocket.(*ADBSocket); ok {
		frame, err := adbSocket.ReadH264Stream()
		if err != nil {
			return frame, false, false, err
		}

		// 获取配置帧和关键帧标志
		isConfig := adbSocket.lastIsConfig
		isKeyFrame := adbSocket.lastIsKeyFrame

		// 将PTS信息编码到帧数据前（临时方案，避免修改太多接口）
		// 注意：这会在帧数据前添加8字节的PTS信息
		pts := adbSocket.lastPTS
		ptsBytes := make([]byte, 8)
		ptsBytes[0] = byte(pts >> 56)
		ptsBytes[1] = byte(pts >> 48)
		ptsBytes[2] = byte(pts >> 40)
		ptsBytes[3] = byte(pts >> 32)
		ptsBytes[4] = byte(pts >> 24)
		ptsBytes[5] = byte(pts >> 16)
		ptsBytes[6] = byte(pts >> 8)
		ptsBytes[7] = byte(pts)

		// 在帧数据前添加PTS信息（8字节）
		frameWithPTS := make([]byte, 8+len(frame))
		copy(frameWithPTS[:8], ptsBytes)
		copy(frameWithPTS[8:], frame)

		return frameWithPTS, isConfig, isKeyFrame, nil
	}

	return nil, false, false, fmt.Errorf("socket类型错误")
}

// CloseVideoSocketInterrupt 关闭视频 TCP，并 Close 控制 TCP（若存在），打断视频读与控制读；不 pkill、不移 forward。
// 视频 socket 置 nil；控制连接勿在此置 nil，由 handleControlStream 读退出后 teardown 统一清理。
// 注意：池内同设备若还有其它功能共用本 ScrcpyServer 的控制连接，关控制会一并打断，需依赖后续 Start() 重连。
// 池化场景下下一轮 AndroidStreamer.Start 会通过 HasVideoSocket==false 触发 Start() 全量重连。
func (s *ScrcpyServer) CloseVideoSocketInterrupt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.videoSocket != nil {
		logutil.Debugf("[scrcpy-server] 设备 %s: CloseVideoSocketInterrupt 关闭视频 socket", s.deviceUDID)
		_ = s.videoSocket.Close()
		s.videoSocket = nil
	}
	if s.controlConn != nil {
		logutil.Debugf("[scrcpy-server] 设备 %s: CloseVideoSocketInterrupt 关闭控制连接", s.deviceUDID)
		_ = s.controlConn.Close()
	}
}

// HasVideoSocket 是否仍有视频 socket（供池化路径判断是否需要 Start 重连）。
func (s *ScrcpyServer) HasVideoSocket() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.videoSocket != nil
}

// RequestKeyFrame 发送 RESET_VIDEO（0x11）。应在 scrcpy 已运行且调用端链路就绪后使用（如 WebRTC Connected）；过早调用可能触发设备端 capture NPE。
func (s *ScrcpyServer) RequestKeyFrame() error {
	if err := s.SendControlMessage([]byte{0x11}); err != nil {
		logutil.Errorf("[scrcpy-server] ❌ 请求关键帧失败: %v", err)
		return fmt.Errorf("请求关键帧失败: %v", err)
	}
	return nil
}

// SetVideoEnabled 官方 scrcpy-server（3.3.x）控制协议不含 TYPE_SET_VIDEO_ENABLED(0x12)，发送会导致 ControlMessageReader 异常。
func (s *ScrcpyServer) SetVideoEnabled(enabled bool) error {
	_ = enabled
	return nil
}

// SetAudioEnabled 官方 scrcpy-server 不含 TYPE_SET_AUDIO_ENABLED(0x13)。
func (s *ScrcpyServer) SetAudioEnabled(enabled bool) error {
	_ = enabled
	return nil
}

// SendControlMessage 将一帧控制协议数据入队，由唯一写协程顺序写出（对齐 controller.c sc_controller_push_msg + run_controller）。
func (s *ScrcpyServer) SendControlMessage(data []byte) (err error) {
	if len(data) == 0 {
		return fmt.Errorf("控制消息为空")
	}
	// 前端 WebRTC 心跳 0xFF；Java ControlMessageReader 无此 type 会抛 Unknown event type → 整会话退出。
	if len(data) == 1 && data[0] == 0xFF {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("控制通道已关闭")
		}
	}()

	s.mu.Lock()
	running := s.running
	ch := s.controlOut
	s.mu.Unlock()
	if !running {
		return fmt.Errorf("scrcpy-server未运行")
	}
	s.ensureControlLoopsStarted()
	if ch == nil {
		s.mu.Lock()
		ch = s.controlOut
		s.mu.Unlock()
		if ch == nil {
			return fmt.Errorf("控制通道尚未就绪")
		}
	}
	dup := make([]byte, len(data))
	copy(dup, data)
	if isDroppableControlMessageType(dup[0]) && len(ch) >= scControlMsgQueueLimit {
		// 对齐 controller.c: 队列达到 LIMIT 后丢弃 droppable 消息；
		// 不可丢弃消息可继续进入额外保留槽位。
		return nil
	}
	select {
	case ch <- dup:
	default:
		if isDroppableControlMessageType(dup[0]) {
			// 对齐 controller.c：队列满时可丢弃 droppable 消息。
			return nil
		}
		// 不可丢弃消息（UHID_CREATE/DESTROY）阻塞等待入队。
		ch <- dup
	}
	return nil
}

// stopUnlocked 在已持 s.mu 时调用；会短暂释放锁以等待控制写线程（对齐 sc_controller_join），再持锁清理 TCP/shell。
func (s *ScrcpyServer) stopUnlocked() {
	if !s.running {
		return
	}

	logutil.Debugf("[scrcpy-server] 开始停止服务...")
	select {
	case s.stopChan <- struct{}{}:
		logutil.Debugf("[scrcpy-server] 已发送停止信号")
	default:
	}

	// 先关闭控制 TCP，使写线程中阻塞的 Write 返回（避免仅 close(channel) 时写协程仍卡在 Write 上导致 Wait 死锁）
	if s.controlConn != nil {
		_ = s.controlConn.Close()
		s.controlConn = nil
	}
	s.controlReaderStarted = false

	ch := s.controlOut
	s.controlOut = nil
	s.mu.Unlock()
	if ch != nil {
		close(ch)
	}
	s.controlWg.Wait()
	s.mu.Lock()

	if s.forwardPort != 0 && s.adbDevice != nil {
		localSpec := adb.ForwardSpec{Protocol: adb.FProtocolTcp, PortOrName: strconv.Itoa(s.forwardPort)}
		_ = s.adbDevice.ForwardRemove(localSpec)
		s.forwardPort = 0
		logutil.Debugf("[scrcpy-server] 已移除 ADB forward")
	}

	if s.videoSocket != nil {
		logutil.Debugf("[scrcpy-server] 关闭视频socket连接...")
		if err := s.videoSocket.Close(); err != nil {
			logutil.Errorf("[scrcpy-server] 关闭视频socket失败: %v", err)
		}
		s.videoSocket = nil
	}

	if s.scrcpyShellConn != nil {
		_ = s.scrcpyShellConn.Close()
		s.scrcpyShellConn = nil
	}
	logutil.Debugf("[scrcpy-server] 使用pkill停止进程...")
	s.pkillScrcpyJavaServer()

	s.running = false
	logutil.Debugf("[scrcpy-server] 服务已停止")
}

// Stop 停止连接并清理资源。若 Start() 仍在连接/重试中，会发送中止信号使其尽快退出并清理。
func (s *ScrcpyServer) Stop() error {
	s.mu.Lock()
	if !s.running {
		select {
		case s.startAbortChan <- struct{}{}:
			logutil.Debugf("[scrcpy-server] 已发送启动中止信号")
		default:
		}
		s.mu.Unlock()
		return nil
	}

	s.stopUnlocked()
	s.mu.Unlock()
	return nil
}

// IsRunning 检查是否运行中
func (s *ScrcpyServer) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// scrcpyServerOutput 用于捕获server输出
type scrcpyServerOutput struct {
	prefix string
}

func (o *scrcpyServerOutput) Write(p []byte) (n int, err error) {
	if !logutil.DebugEnabled() {
		return len(p), nil
	}
	output := string(p)
	output = strings.TrimRight(output, "\r\n")
	if output != "" {
		if strings.Contains(o.prefix, "err") {
			log.Printf("%s设备输出: %s", o.prefix, output)
			return len(p), nil
		}
		if strings.Contains(output, "Video capture reset") ||
			strings.Contains(output, "Display: using") ||
			strings.Contains(output, "Audio encoder stopped") ||
			strings.Contains(output, "DEBUG:") {
			return len(p), nil
		}
		lowerOutput := strings.ToLower(output)
		if strings.Contains(lowerOutput, "error") ||
			strings.Contains(lowerOutput, "exception") ||
			strings.Contains(lowerOutput, "fail") ||
			strings.Contains(lowerOutput, "warning") ||
			strings.Contains(lowerOutput, "touch") ||
			strings.Contains(lowerOutput, "control") {
			log.Printf("%s⚠️ %s", o.prefix, output)
			return len(p), nil
		}
		log.Printf("%s%s", o.prefix, output)
	}
	return len(p), nil
}
