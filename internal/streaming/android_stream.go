package streaming

import (
	"context"
	"embed"
	"fmt"
	"github.com/ms-robots/ms-robot/internal/device"
	"github.com/ms-robots/ms-robot/internal/logutil"
	"github.com/ms-robots/ms-robot/third_party/adb"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

// AndroidStreamer Android设备视频流
type AndroidStreamer struct {
	device         *device.Device
	adbDevice      *adb.Device
	scrcpyServer   *ScrcpyServer
	scrcpyFromPool bool                                      // true 表示 scrcpy 来自池，关闭投屏时不退出 scrcpy
	audioTracks    map[string]*webrtc.TrackLocalStaticSample // sessionID -> 音频轨 (Opus)
	// 视频订阅表仅在 orchestrator goroutine 内修改（经 streamCtrl 消息），无 mutex
	streamCtrl          chan streamOrchestratorMsg // nil 表示未起转发协程
	orchWG              sync.WaitGroup             // Stop 时等待 orchestrator 退出
	ctx                 context.Context
	cancel              context.CancelFunc
	frameRate           int  // 帧率
	useScrcpy           bool // 是否使用scrcpy
	mu                  sync.Mutex
	running             bool             // Start 全路径成功（含 demux gate 打开）后为 true
	starting            bool             // Start 执行中，防止并发 Start；此时 IsRunning()==false
	reading             bool             // 是否正在读取视频帧
	readStopChan        chan struct{}    // 用于停止读取goroutine
	frameChan           chan *VideoFrame // 读协程 -> orchestrator
	consumerCount       int32            // 仅由 orchestrator 原子写；读协程原子读
	onDisconnect        func()           // 断开连接回调函数
	logAudioNoTrackNext time.Time        // 限频：下次打「无音频轨」日志时间
}

// streamOrchestratorMsg 仅由单条 orchestrator goroutine 在收到后改本地 map（无锁表）
type streamOrchestratorMsg interface{}

type orchAddWebRTC struct {
	sessionID string
	ch        chan *VideoFrame
	done      chan struct{} // 关闭表示已写入 map（同步 Register）
}

type orchRemoveWebRTC struct {
	sessionID string
	done      chan struct{} // 可选同步
}

type orchPipelineActive struct {
	active bool
}

// VideoFrame 视频帧数据
type VideoFrame struct {
	Data       []byte
	IsConfig   bool
	IsKeyFrame bool
}

// NewAndroidStreamer 创建 Android 流。scrcpyServer 为 nil 时内部创建（旧逻辑）；非 nil 时使用池中常驻 scrcpy（关闭投屏不退出 scrcpy）。
func NewAndroidStreamer(deviceManager *device.Manager, dev *device.Device, adbDevice *adb.Device, assetsFS embed.FS, scrcpyServer *ScrcpyServer) (*AndroidStreamer, error) {
	ctx, cancel := context.WithCancel(context.Background())

	streamer := &AndroidStreamer{
		device:         dev,
		adbDevice:      adbDevice,
		ctx:            ctx,
		cancel:         cancel,
		frameRate:      30,   // 默认30fps
		useScrcpy:      true, // 优先使用scrcpy
		audioTracks:    make(map[string]*webrtc.TrackLocalStaticSample),
		readStopChan:   make(chan struct{}),
		frameChan:      make(chan *VideoFrame, 4), // 缓冲4帧（约133ms@30fps），配合关键帧丢弃策略，降低延迟
		onDisconnect:   nil,                       // 由 WebRTCManager 设置
		scrcpyFromPool: scrcpyServer != nil,
	}
	onDeviceDisconnect := func() {
		streamer.mu.Lock()
		cb := streamer.onDisconnect
		streamer.mu.Unlock()
		if cb != nil {
			logutil.Infof("[AndroidStreamer] 设备 %s: scrcpy-server通知连接断开，立即触发断开", dev.UDID)
			cb()
		}
	}
	onDeviceEnded := func(reason string) {
		switch reason {
		case "controller_error":
			logutil.Warnf("[AndroidStreamer] 设备 %s: scrcpy 控制链路结束原因=controller_error", dev.UDID)
		case "device_disconnected":
			logutil.Infof("[AndroidStreamer] 设备 %s: scrcpy 控制链路结束原因=device_disconnected", dev.UDID)
		default:
			logutil.Warnf("[AndroidStreamer] 设备 %s: scrcpy 控制链路结束原因=%s", dev.UDID, reason)
		}
	}

	if scrcpyServer != nil {
		streamer.scrcpyServer = scrcpyServer
		scrcpyServer.SetOnDeviceDisconnect(onDeviceDisconnect)
		scrcpyServer.SetOnDeviceEnded(onDeviceEnded)
		return streamer, nil
	}

	adbClient := deviceManager.AdbClient()
	dmDial := deviceManager.GetDialTCP()
	var dialTCP dialTCPFunc
	if dmDial != nil {
		dialTCP = func(addr string) (net.Conn, error) { return dmDial(addr) }
	}
	server, err := NewScrcpyServer(adbDevice, adbClient, dev.UDID, assetsFS, deviceManager.AdbHost(), dialTCP)
	if err != nil {
		logutil.Infof("创建scrcpy-server失败，将使用截图方案: %v", err)
		streamer.useScrcpy = false
	} else {
		streamer.scrcpyServer = server
		server.SetOnDeviceDisconnect(onDeviceDisconnect)
		server.SetOnDeviceEnded(onDeviceEnded)
	}

	return streamer, nil
}

// SetScrcpyOptions 设置 scrcpy 启动参数（需在 Start 前调用）；0 表示不传该参数
func (s *AndroidStreamer) SetScrcpyOptions(maxSize, videoBitRate int) {
	if s.scrcpyServer != nil {
		s.scrcpyServer.SetMaxSize(maxSize)
		s.scrcpyServer.SetVideoBitRate(videoBitRate)
	}
}

// Start 启动视频流（第一个连接时调用）。running 仅在 demux gate 打开、本路读取就绪前一刻才置 true，此前 IsRunning()==false。
func (s *AndroidStreamer) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("流已在运行")
	}
	if s.starting {
		s.mu.Unlock()
		return fmt.Errorf("流正在启动中")
	}
	s.starting = true

	if s.useScrcpy && s.scrcpyServer != nil {
		if s.scrcpyFromPool {
			// 池中 scrcpy 已常驻，只发启视频/音频命令
			if !s.scrcpyServer.IsRunning() {
				// 池实例可能因严格对齐官方语义（控制流EOF即结束）被停掉；此处按池语义自动拉起。
				if err := s.scrcpyServer.Start(); err != nil {
					s.starting = false
					s.mu.Unlock()
					return fmt.Errorf("scrcpy 未运行且重启失败: %v", err)
				}
			} else if !s.scrcpyServer.HasVideoSocket() {
				// Stop 时 CloseVideoSocketInterrupt 关了视频 TCP，IsRunning 仍为 true，须完整重连
				if err := s.scrcpyServer.Start(); err != nil {
					s.starting = false
					s.mu.Unlock()
					return fmt.Errorf("scrcpy 视频通道重连失败: %v", err)
				}
			}
			_ = s.scrcpyServer.SetVideoEnabled(true)
		} else {
			if err := s.startScrcpyStream(); err != nil {
				s.starting = false
				s.mu.Unlock()
				return fmt.Errorf("启动scrcpy流失败: %v", err)
			}
		}
		if s.scrcpyServer != nil && s.scrcpyServer.EnableAudio() {
			s.scrcpyServer.SetAudioSink(s)
			_ = s.scrcpyServer.SetAudioEnabled(true)
		}
	} else {
		s.starting = false
		s.mu.Unlock()
		return fmt.Errorf("scrcpy-server未初始化，无法启动视频流")
	}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
	}()

	// 对齐 scrcpy.c 阶段顺序：
	// 1) server_connect_to 返回之后：demuxer/decoder 等 init（无 socket 读）— 此处起读/发协程但读侧在 gate 上阻塞，等价「未 sc_demuxer_start」
	// 2) sc_controller_start — ensureControlLoopsStarted
	// 3) sc_screen_init 等 — afterControlBeforeVideoDemux
	// 4) sc_demuxer_start — close(gate)，读侧才首次 ReadVideoStream
	videoDemuxGate := make(chan struct{})
	if err := s.startVideoPipelineWithGate(videoDemuxGate); err != nil {
		return fmt.Errorf("启动读取失败: %v", err)
	}
	if s.scrcpyServer != nil {
		s.scrcpyServer.ensureControlLoopsStarted()
	}
	s.afterControlBeforeVideoDemux()
	close(videoDemuxGate)

	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return fmt.Errorf("流已停止")
	}
	s.running = true
	s.mu.Unlock()
	return nil
}

// postStreamCtrl 将消息交给唯一 orchestrator；无转发协程或超时时返回 false。
func (s *AndroidStreamer) postStreamCtrl(m streamOrchestratorMsg) bool {
	s.mu.Lock()
	c := s.streamCtrl
	s.mu.Unlock()
	if c == nil {
		return false
	}
	select {
	case c <- m:
		return true
	case <-time.After(5 * time.Second):
		logutil.Warnf("[AndroidStreamer] streamCtrl 投递超时，可能转发协程已退出")
		return false
	}
}

// SetWebRTCVideoPipelineActive 由 streamSupervisor 在 WebRTC Reserve/teardown 时设置：Reserve 至 register 期间仍消费视频以排空隧道。
func (s *AndroidStreamer) SetWebRTCVideoPipelineActive(active bool) {
	_ = s.postStreamCtrl(&orchPipelineActive{active: active})
}

// RegisterWebRTCVideoSub 登记该会话专属帧 chan；scrcpy→转发协程只向此 chan 非阻塞写拷贝。
// 同 sessionID 重入前须先 RemoveWebRTCVideoSub（由 supervisor stopVideoPeer 保证）。
func (s *AndroidStreamer) RegisterWebRTCVideoSub(sessionID string, ch chan *VideoFrame) {
	done := make(chan struct{})
	if !s.postStreamCtrl(&orchAddWebRTC{sessionID: sessionID, ch: ch, done: done}) {
		return
	}
	<-done
}

// RemoveWebRTCVideoSub 摘除订阅并 close(chan)，对端 webrtc 读协程退出。
func (s *AndroidStreamer) RemoveWebRTCVideoSub(sessionID string) {
	done := make(chan struct{})
	if !s.postStreamCtrl(&orchRemoveWebRTC{sessionID: sessionID, done: done}) {
		return
	}
	<-done
}

// IsRunning 是否已完成 Start（demux 已开读）；Start 执行中为 false。
func (s *AndroidStreamer) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// WriteAudio 实现 AudioSink，将设备端 Opus 转发到所有音频轨
func (s *AndroidStreamer) WriteAudio(data []byte, duration time.Duration) {
	if len(data) == 0 {
		return
	}
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	tracks := make(map[string]*webrtc.TrackLocalStaticSample, len(s.audioTracks))
	for k, t := range s.audioTracks {
		tracks[k] = t
	}
	nTracks := len(tracks)
	s.mu.Unlock()
	if nTracks == 0 {
		// 限频：仅首次或每 5 秒打一次，避免刷屏
		if s.logAudioNoTrackNext.Before(time.Now()) {
			logutil.Debugf("[AndroidStreamer] [音频] 收到设备数据 %d 字节，但当前无音频轨 (audioTracks=0)，前端收不到音频轨", len(data))
			s.logAudioNoTrackNext = time.Now().Add(5 * time.Second)
		}
		return
	}
	sample := media.Sample{Data: data, Duration: duration}
	for _, t := range tracks {
		_ = t.WriteSample(sample)
	}
}

// AddAudioTrack 添加 Opus 音频轨（每 WebRTC 会话一条）
func (s *AndroidStreamer) AddAudioTrack(sessionID string, track *webrtc.TrackLocalStaticSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audioTracks[sessionID] = track
	logutil.Infof("[AndroidStreamer] 已添加音频轨 (sessionID: %s)", sessionID)
}

// RemoveAudioTrack 移除音频轨
func (s *AndroidStreamer) RemoveAudioTrack(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.audioTracks, sessionID)
}

// Stop 停止视频流
func (s *AndroidStreamer) Stop() {
	s.mu.Lock()
	// Start() 在置 running=true 前会先起读协程与 orchestrator（starting==true, reading 可能已为 true）。
	// 仅判断 running 会在此窗口内直接 return，导致读协程/scrcpy 未停、重连双读或池状态脏。
	if !s.running && !s.starting && !s.reading {
		s.mu.Unlock()
		return
	}

	// 先结束「运行」语义并摘掉 scrcpy 音频 sink：池化 scrcpy 不会 Stop()，音频读协程仍可能推包；
	// 若不 SetAudioSink(nil)，会继续回调本实例且 audioTracks 已空，从而每 5 秒打「无音频轨」日志。
	s.running = false
	s.starting = false
	s.cancel() // 先于停读：读协调与 Start() 末尾可立刻看到 Done，避免 Stop 与 Start 交错把 running 又置回 true
	if s.scrcpyServer != nil {
		// 打断阻塞在 ReadFull 的读 pump（与 ctx/readStop 无关，须关视频连接）
		s.scrcpyServer.CloseVideoSocketInterrupt()
		s.scrcpyServer.SetAudioSink(nil)
		s.scrcpyServer.SetDeviceMessageHandler(nil) // 避免已退役 supervisor 仍往 ctrl 投递设备上行
	}

	s.audioTracks = make(map[string]*webrtc.TrackLocalStaticSample)

	hadVideoPipeline := s.reading
	// 先停读协程，使其 defer 关闭 frameChan；orchestrator 在 frameChan 关闭后关订阅并 orchWG.Done
	if s.reading {
		select {
		case <-s.readStopChan:
		default:
			close(s.readStopChan)
		}
		s.reading = false
		s.readStopChan = make(chan struct{})
	}
	s.mu.Unlock()

	if hadVideoPipeline {
		s.orchWG.Wait()
	}

	s.mu.Lock()

	if s.scrcpyServer != nil {
		_ = s.scrcpyServer.SetVideoEnabled(false)
		_ = s.scrcpyServer.SetAudioEnabled(false)
	}

	// 仅当 scrcpy 非池中常驻时才退出 scrcpy；来自池时只停读流，scrcpy 不退出
	if s.scrcpyServer != nil && !s.scrcpyFromPool {
		s.scrcpyServer.Stop()
	}
	s.mu.Unlock()

	logutil.Infof("[AndroidStreamer] 视频流已完全停止，所有资源已清理")
}

// startScrcpyStream 启动scrcpy-server并建立连接，读取设备信息和视频头。
// 实际视频读取在 Start() 中立即启动（通过 StartReading）。
func (s *AndroidStreamer) startScrcpyStream() error {
	logutil.Infof("[AndroidStreamer] 开始启动scrcpy-server...")

	// 启动 scrcpy-server（Start() 会阻塞直到连接建立）
	// 按照 test_scrcpy_stream.go，Start() 会同步执行，Accept() 会阻塞等待连接
	// Start() 内部会读取设备信息和视频头（这些必须立即读取）
	if err := s.scrcpyServer.Start(); err != nil {
		logutil.Errorf("[AndroidStreamer] 设备 %s: scrcpyServer.Start() 失败: %v", s.device.UDID, err)
		return fmt.Errorf("启动scrcpy-server失败: %v", err)
	}

	logutil.Infof("[AndroidStreamer] scrcpy-server已启动，连接已就绪")

	return nil
}

// afterControlBeforeVideoDemux 对齐 scrcpy.c 中 sc_controller_start 与 sc_demuxer_start 之间的
// sc_screen_init、sc_audio_player_init、v4l2 等。无 SDL 投屏窗口时为空操作，需要时可在此挂接 UI/解码器就绪逻辑。
func (s *AndroidStreamer) afterControlBeforeVideoDemux() {
	_ = s
}

// scrcpyReadPumpMsg：独立 goroutine 只做阻塞 ReadVideoStream，结果经 chan 交给协调侧 select（与 ctx/readStop 同优先级）。
type scrcpyReadPumpMsg struct {
	payload              []byte
	isConfig, isKeyFrame bool
	err                  error
}

// startVideoPipelineWithGate 启动视频读/发协程。若 videoDemuxGate 非 nil，读协程在首次 ReadVideoStream 前阻塞于该 channel，
// 直至调用方 close(gate)，等价官方「先 sc_controller_start，再 sc_demuxer_start」。传 nil 则立即读视频（旁路/二次会话）。
func (s *AndroidStreamer) startVideoPipelineWithGate(videoDemuxGate <-chan struct{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Start() 中途会调用本函数，此时 running 尚未置 true，仅 starting==true；HTTP/WebRTC 后续 StartReading 则要求 running==true。
	if !s.running && !s.starting {
		return fmt.Errorf("流未运行，无法开始读取")
	}
	if s.reading {
		return fmt.Errorf("视频帧读取已在进行中")
	}
	if s.scrcpyServer == nil || !s.scrcpyServer.IsRunning() {
		return fmt.Errorf("scrcpy-server未运行，无法读取视频帧")
	}

	s.frameChan = make(chan *VideoFrame, 4)
	ctrlCh := make(chan streamOrchestratorMsg, 64)
	s.streamCtrl = ctrlCh
	s.reading = true
	logutil.Infof("[AndroidStreamer] 开始读取视频帧...")

	// 协程 A：仅阻塞 ReadVideoStream → readPumpCh；协程 B：select ctx/readStop/readPumpCh 并写 frameChan（Stop 时 CloseVideoSocket 可立刻打断 Read）
	go func() {
		defer func() {
			close(s.frameChan)
			s.mu.Lock()
			s.reading = false
			s.mu.Unlock()
			logutil.Infof("[AndroidStreamer] 视频读协调退出")
		}()

		consecutiveErrors := 0
		maxConsecutiveErrors := 100
		firstConfigLogged := false
		firstKeyFrameLogged := false

		if videoDemuxGate != nil {
			select {
			case <-s.ctx.Done():
				return
			case <-s.readStopChan:
				return
			case <-videoDemuxGate:
			}
		}

		readPumpCh := make(chan scrcpyReadPumpMsg, 8)
		srv := s.scrcpyServer
		go func() {
			defer close(readPumpCh)
			for {
				if srv == nil {
					return
				}
				frame, isConfig, isKeyFrame, err := srv.ReadVideoStream()
				msg := scrcpyReadPumpMsg{payload: frame, isConfig: isConfig, isKeyFrame: isKeyFrame, err: err}
				if err != nil && strings.Contains(err.Error(), "视频socket未建立") {
					select {
					case readPumpCh <- msg:
					case <-s.ctx.Done():
					}
					return
				}
				select {
				case readPumpCh <- msg:
				case <-s.ctx.Done():
					return
				}
			}
		}()

		logutil.Infof("[AndroidStreamer] [读协调] 开始消费 scrcpy 视频帧...")

	coordLoop:
		for {
			select {
			case <-s.ctx.Done():
				logutil.Infof("[AndroidStreamer] [读协调] 收到停止信号，退出")
				return
			case <-s.readStopChan:
				logutil.Infof("[AndroidStreamer] [读协调] 收到停止读取信号，退出")
				return
			case r, ok := <-readPumpCh:
				if !ok {
					logutil.Infof("[AndroidStreamer] [读协调] read pump 已结束")
					return
				}
				if r.err != nil {
					errStr := r.err.Error()
					if errStr == "暂时无数据" {
						if !s.scrcpyServer.IsRunning() {
							logutil.Infof("[AndroidStreamer] scrcpy-server已停止，视频流结束")
							return
						}
						time.Sleep(100 * time.Millisecond)
						continue coordLoop
					}
					if r.err == io.EOF {
						if !s.scrcpyServer.IsRunning() {
							logutil.Infof("[AndroidStreamer] scrcpy-server已停止，视频流结束")
							return
						}
						consecutiveErrors++
						if consecutiveErrors >= 10 {
							logutil.Infof("[AndroidStreamer] ⚠️ 连续读取失败 %d 次（设备可能已断开），停止视频流", consecutiveErrors)
							s.scrcpyServer.Stop()
							return
						}
						if consecutiveErrors%100 == 1 {
							logutil.Infof("[AndroidStreamer] 等待视频流数据... (连续 %d 次 EOF)", consecutiveErrors)
						}
						time.Sleep(100 * time.Millisecond)
						continue coordLoop
					}
					if strings.Contains(errStr, "视频socket未建立") || strings.Contains(errStr, "server未运行") {
						return
					}
					consecutiveErrors++
					if consecutiveErrors >= maxConsecutiveErrors {
						logutil.Infof("[AndroidStreamer] ⚠️ 连续读取失败 %d 次，停止视频流: %v", maxConsecutiveErrors, r.err)
						return
					}
					if consecutiveErrors%100 == 1 {
						logutil.Infof("[AndroidStreamer] 读取视频流失败 (连续 %d 次): %v", consecutiveErrors, r.err)
					}
					time.Sleep(100 * time.Millisecond)
					continue coordLoop
				}

				if consecutiveErrors > 0 {
					logutil.Infof("[AndroidStreamer] ✓ 恢复读取，已成功读取数据（之前连续错误: %d 次）", consecutiveErrors)
				}
				consecutiveErrors = 0

				frame := r.payload
				isConfig := r.isConfig
				isKeyFrame := r.isKeyFrame

				if !s.scrcpyServer.IsRunning() {
					logutil.Infof("[AndroidStreamer] [读协调] scrcpy-server已停止运行，退出")
					return
				}

				if len(frame) == 0 {
					continue coordLoop
				}

				if isConfig && !firstConfigLogged {
					logutil.Infof("[AndroidStreamer] 首次收到配置帧: size=%d", len(frame))
					firstConfigLogged = true
				}
				if isKeyFrame && !firstKeyFrameLogged {
					logutil.Infof("[AndroidStreamer] 首次收到关键帧: size=%d", len(frame))
					firstKeyFrameLogged = true
				}

				if atomic.LoadInt32(&s.consumerCount) == 0 {
					continue coordLoop
				}

				if isKeyFrame {
					droppedCount := 0
					keptFrames := make([]*VideoFrame, 0, 8)

					for {
						select {
						case oldFrame := <-s.frameChan:
							if oldFrame.IsKeyFrame || oldFrame.IsConfig {
								keptFrames = append(keptFrames, oldFrame)
							} else {
								droppedCount++
							}
						default:
							goto sendKeyFrame
						}
					}

				sendKeyFrame:
					if len(keptFrames) > 2 {
						keptFrames = keptFrames[len(keptFrames)-2:]
					}
					for _, keptFrame := range keptFrames {
						select {
						case s.frameChan <- keptFrame:
						default:
						}
					}

					_ = droppedCount

					s.frameChan <- &VideoFrame{
						Data:       frame,
						IsConfig:   isConfig,
						IsKeyFrame: isKeyFrame,
					}
				} else if isConfig {
					s.frameChan <- &VideoFrame{
						Data:       frame,
						IsConfig:   isConfig,
						IsKeyFrame: isKeyFrame,
					}
				} else {
					select {
					case s.frameChan <- &VideoFrame{
						Data:       frame,
						IsConfig:   isConfig,
						IsKeyFrame: isKeyFrame,
					}:
					case <-time.After(8 * time.Millisecond):
					}
				}
			}
		}
	}()

	// Goroutine 2: 唯一 orchestrator — 本地 subs/pipeActive，仅经 ctrlCh 变更 map，无 mutex
	s.orchWG.Add(1)
	go func() {
		subs := make(map[string]chan *VideoFrame)
		pipeActive := false

		recalc := func() {
			n := 0
			if len(subs) > 0 || pipeActive {
				n = 1
			}
			atomic.StoreInt32(&s.consumerCount, int32(n))
		}

		releaseAll := func() {
			for sid, ch := range subs {
				delete(subs, sid)
				close(ch)
			}
			pipeActive = false
			atomic.StoreInt32(&s.consumerCount, 0)
		}

		defer func() {
			releaseAll()
			s.mu.Lock()
			if s.streamCtrl == ctrlCh {
				s.streamCtrl = nil
			}
			s.mu.Unlock()
			s.orchWG.Done()
			logutil.Infof("[AndroidStreamer] [orchestrator] 退出")
		}()

		logutil.Infof("[AndroidStreamer] [orchestrator] frameChan → webrtcSubs…")

		for {
			select {
			case m := <-ctrlCh:
				switch msg := m.(type) {
				case *orchAddWebRTC:
					if old, ok := subs[msg.sessionID]; ok {
						close(old)
					}
					subs[msg.sessionID] = msg.ch
					recalc()
					if msg.done != nil {
						close(msg.done)
					}
				case *orchRemoveWebRTC:
					if ch, ok := subs[msg.sessionID]; ok {
						delete(subs, msg.sessionID)
						close(ch)
					}
					recalc()
					if msg.done != nil {
						close(msg.done)
					}
				case *orchPipelineActive:
					pipeActive = msg.active
					recalc()
				default:
					logutil.Warnf("[AndroidStreamer] orchestrator: 未知消息类型 %T", m)
				}
			case frame, ok := <-s.frameChan:
				if !ok {
					return
				}
				if frame == nil || len(frame.Data) == 0 {
					continue
				}
				for _, ch := range subs {
					cp := &VideoFrame{
						Data:       append([]byte(nil), frame.Data...),
						IsConfig:   frame.IsConfig,
						IsKeyFrame: frame.IsKeyFrame,
					}
					select {
					case ch <- cp:
					default:
					}
				}
			}
		}
	}()

	logutil.Infof("[AndroidStreamer] 视频帧读取与转发 goroutine 已启动")
	return nil
}

// StartReading 开始读取视频帧（在 WebRTC 后续会话等场景调用）。不经 Start() 首连 gate 时 gate=nil，读侧立即 ReadVideoStream。
func (s *AndroidStreamer) StartReading() error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return fmt.Errorf("流未运行，无法开始读取")
	}
	if s.reading {
		s.mu.Unlock()
		logutil.Infof("[AndroidStreamer] 视频帧读取已在进行中")
		return nil
	}
	if s.scrcpyServer == nil || !s.scrcpyServer.IsRunning() {
		s.mu.Unlock()
		return fmt.Errorf("scrcpy-server未运行，无法读取视频帧")
	}
	s.mu.Unlock()
	return s.startVideoPipelineWithGate(nil)
}

// 已移除 captureLoop 和 captureAndSend 函数
// 现在只使用 scrcpy-server 获取 H.264 视频流，不再使用 adb exec-out screencap 截图

// StopReading 停止读取视频帧（但保持scrcpy-server运行）
// 可以用于暂停视频流，节省资源
func (s *AndroidStreamer) StopReading() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.reading {
		logutil.Infof("[AndroidStreamer] 视频帧读取未在进行中，无需停止")
		return nil
	}

	logutil.Infof("[AndroidStreamer] 停止读取视频帧...")

	// 发送停止信号
	select {
	case <-s.readStopChan:
		// 已经关闭，不需要操作
	default:
		close(s.readStopChan)
	}
	s.reading = false

	// 重新创建 channel，以便下次使用
	s.readStopChan = make(chan struct{})

	logutil.Infof("[AndroidStreamer] 视频帧读取已停止（scrcpy-server仍在运行）")
	return nil
}

// IsReading 检查是否正在读取视频帧
func (s *AndroidStreamer) IsReading() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reading
}

// GetScrcpyServer 获取scrcpy-server实例（用于发送控制命令）
func (s *AndroidStreamer) GetScrcpyServer() *ScrcpyServer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scrcpyServer
}

// SetOnDisconnect 设置断开连接回调函数
func (s *AndroidStreamer) SetOnDisconnect(callback func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onDisconnect = callback
}
