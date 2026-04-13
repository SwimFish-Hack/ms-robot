package streaming

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ms-robots/ms-robot/internal/logutil"

	webrtc "github.com/pion/webrtc/v3"
)

// 每路 device 一条 supervisor goroutine：仅处理 ctrl；该设备投屏消费者全空时从 streamSupervisors 摘除并退出本 goroutine，下次再连会新建。
// 视频：scrcpy→AndroidStreamer 转发协程只向各会话 chan 写帧；每会话独立 goroutine 读 chan + H264Streamer.ProcessH264Frame。

const streamSupCtrlCap = 4096

// errSupervisorRetired：teardown 后 retire 排空 ctrl 时对 Reserve/挂轨的回复，finishScrcpyAttach 收到后立即摘会话。
var errSupervisorRetired = errors.New("stream supervisor retired")

// 每路 WebRTC 视频轨缓冲深度：满则丢帧，不阻塞 supervisor。
const supVideoPeerFrameCap = 3

type supVideoPeer struct {
	h264     *H264Streamer
	peerConn *webrtc.PeerConnection
}

type streamSupervisor struct {
	mgr        *WebRTCManager
	key        string
	ctrl       chan streamSupMsg
	runDone    chan struct{} // run 退出时 close；tryWebRTCRelease 与之 select，避免 run 已停仍阻塞在 ctrl 上
	once       sync.Once
	videoPeers map[string]*supVideoPeer
	retired    int32 // 1=已从 streamSupervisors 摘除，run 处理完当前消息后退出

	// 多路同时 Reserve 时共等一次 Start()；挂轨以 msgRegisterVideoPeer 内 IsRunning() 为准（Start 全路径成功后才为 true）。
	scrcpyFullyStarted   bool
	scrcpyWaitBroadcast  chan error // 容量足够容纳同批 waiters；Start 结束后向每人发一条结果
	scrcpyWaitersPending int
}

type streamSupMsg interface{}

type msgWebRTCReserve struct {
	streamer *AndroidStreamer
	reply    chan msgWebRTCReserveReply
}

type msgWebRTCReserveReply struct {
	scrcpyWait chan error
	first      bool
}

type msgWebRTCRelease struct{}

type msgPeerClose struct {
	sessionID string
}

type msgDeviceDisconnect struct{}

type msgWebRTCControl struct {
	sessionID string
	data      []byte
}

type msgScrcpyDeviceBroadcast struct {
	payload []byte
}

type msgRegisterVideoPeer struct {
	streamer  *AndroidStreamer
	sessionID string
	track     *webrtc.TrackLocalStaticSample
	peerConn  *webrtc.PeerConnection
	reply     chan error
}

// msgScrcpyStartDone 由 goroutine 中 st.Start() 返回后投递，向本批 scrcpyWaitersPending 个等待方各发一条 err（nil 表示成功）
type msgScrcpyStartDone struct {
	err error
}

func (m *WebRTCManager) supervisorFor(key string) *streamSupervisor {
	v, _ := m.streamSupervisors.LoadOrStore(key, &streamSupervisor{
		mgr:     m,
		key:     key,
		ctrl:    make(chan streamSupMsg, streamSupCtrlCap),
		runDone: make(chan struct{}),
	})
	sup := v.(*streamSupervisor)
	sup.once.Do(func() { go sup.run() })
	return sup
}

func (sup *streamSupervisor) streamSessionCount() int {
	v, ok := sup.mgr.streams.Load(sup.key)
	if !ok {
		return 0
	}
	n := 0
	v.(*sync.Map).Range(func(_, _ interface{}) bool {
		n++
		return true
	})
	return n
}

// noConsumersLeft：无 WebRTC 名额占用、无已登记会话（Reserve 与 Store 窗口由 webrtcN 覆盖）
func (sup *streamSupervisor) noConsumersLeft(webrtcN *int) bool {
	return *webrtcN == 0 && sup.streamSessionCount() == 0
}

// abortScrcpyWaitBroadcast 取消正在等待 Start 的 Reserve（每人收到 reason）；并清空「pipeline 已就绪」标志。
func (sup *streamSupervisor) abortScrcpyWaitBroadcast(reason error) {
	if reason == nil {
		reason = errors.New("scrcpy wait cancelled")
	}
	n := sup.scrcpyWaitersPending
	ch := sup.scrcpyWaitBroadcast
	sup.scrcpyWaitersPending = 0
	sup.scrcpyWaitBroadcast = nil
	sup.scrcpyFullyStarted = false
	if ch != nil && n > 0 {
		for i := 0; i < n; i++ {
			select {
			case ch <- reason:
			default:
				logutil.Warnf("[streamSupervisor] %s: scrcpy wait 广播缓冲满，丢弃一条唤醒", sup.key)
			}
		}
	}
}

// removeOneWebRTCSession 从 mgr.streams 摘除指定会话：停 H264 worker、摘音频轨、关 PC、webrtcN--，并在无剩余会话时 teardown。
// 若该 sessionID 已不存在（重复断连回调）返回 false。
func (sup *streamSupervisor) removeOneWebRTCSession(sessionID string, webrtcN *int) bool {
	v, ok := sup.mgr.streams.Load(sup.key)
	if !ok {
		return false
	}
	sm := v.(*sync.Map)
	raw, loaded := sm.LoadAndDelete(sessionID)
	if !loaded {
		return false
	}
	sess := raw.(*StreamSession)
	logutil.Debugf("[WebRTC] 设备 %s 会话 %s: 开始移除流会话（supervisor）...", sup.key, sessionID)
	sup.stopVideoPeer(sessionID)
	if sess.streamer != nil {
		sess.streamer.RemoveAudioTrack(sessionID)
	}
	if sess.PeerConn != nil {
		pc := sess.PeerConn
		// 异步关 PC，避免 pion 在 Close 路径上同步回调 ICE/ConnectionState -> removeStreamSession -> 再向 ctrl 投递时与同一条 supervisor 协程死锁
		go func() { _ = pc.Close() }()
	}
	*webrtcN--
	if *webrtcN < 0 {
		logutil.Warnf("[streamSupervisor] %s: webrtcN 修正（<0）", sup.key)
		*webrtcN = 0
	}
	logutil.Debugf("[WebRTC] 设备 %s 会话 %s: 流会话已移除，webrtcN=%d", sup.key, sessionID, *webrtcN)
	if sup.noConsumersLeft(webrtcN) {
		sup.teardownConsumersLocked(webrtcN)
	}
	return true
}

// drainOneMsgRetired teardown 后退出前排空 ctrl：凡带 reply 的必须应答，否则 webRTCReserve 等永久卡在 <-reply，新 Offer 与 handleDeviceDisconnect 也会堵在 ctrl。
func (sup *streamSupervisor) drainOneMsgRetired(m streamSupMsg, webrtcN *int) {
	switch x := m.(type) {
	case msgWebRTCRelease:
		sup.handleCtrlMsg(m, webrtcN)
	case msgWebRTCReserve:
		ch := make(chan error, 1)
		ch <- errSupervisorRetired
		x.reply <- msgWebRTCReserveReply{scrcpyWait: ch, first: false}
	case msgRegisterVideoPeer:
		x.reply <- fmt.Errorf("%w", errSupervisorRetired)
	case msgPeerClose:
		sup.removeOneWebRTCSession(x.sessionID, webrtcN)
	case msgScrcpyStartDone:
		logutil.Debugf("[streamSupervisor] %s: retire drain 忽略迟到 msgScrcpyStartDone: %v", sup.key, x.err)
	default:
		logutil.Debugf("[streamSupervisor] %s: retire drain 丢弃 %T", sup.key, m)
	}
}

func (sup *streamSupervisor) run() {
	defer close(sup.runDone)
	sup.videoPeers = make(map[string]*supVideoPeer)
	webrtcN := 0

	for {
		msg, ok := <-sup.ctrl
		if !ok {
			return
		}
		sup.handleCtrlMsg(msg, &webrtcN)
		if atomic.LoadInt32(&sup.retired) != 0 {
			// teardown 已置 retired：排空 ctrl 里可能晚到的 msgWebRTCRelease，避免 CreateStream defer 阻塞或 webrtcN 漏减
			for {
				select {
				case m := <-sup.ctrl:
					sup.drainOneMsgRetired(m, &webrtcN)
				default:
					goto supExit
				}
			}
		supExit:
			logutil.Debugf("[streamSupervisor] %s: supervisor goroutine 退出（设备投屏已空）", sup.key)
			return
		}
	}
}

func (sup *streamSupervisor) handleCtrlMsg(msg streamSupMsg, webrtcN *int) {
	switch m := msg.(type) {
	case msgWebRTCReserve:
		*webrtcN++
		first := *webrtcN == 1
		m.streamer.SetWebRTCVideoPipelineActive(true)
		var wait chan error
		if !sup.scrcpyFullyStarted {
			if sup.scrcpyWaitBroadcast == nil {
				// 与同批 waiters 数量一致时可无丢信阻塞投递；容量与 ctrl 一致，极端并发下仍靠阻塞发送与接收端配合
				sup.scrcpyWaitBroadcast = make(chan error, streamSupCtrlCap)
				sup.scrcpyWaitersPending = 0
				st := m.streamer
				go func() {
					e := st.Start()
					sup.ctrl <- msgScrcpyStartDone{err: e}
				}()
			}
			sup.scrcpyWaitersPending++
			wait = sup.scrcpyWaitBroadcast
		}
		m.reply <- msgWebRTCReserveReply{scrcpyWait: wait, first: first}

	case msgScrcpyStartDone:
		n := sup.scrcpyWaitersPending
		ch := sup.scrcpyWaitBroadcast
		if n == 0 && ch == nil {
			if m.err != nil {
				logutil.Debugf("[streamSupervisor] %s: 忽略迟到的 scrcpy Start 结果（已 teardown/cancel）: %v", sup.key, m.err)
			}
			break
		}
		sup.scrcpyWaitersPending = 0
		sup.scrcpyWaitBroadcast = nil
		if m.err == nil {
			sup.scrcpyFullyStarted = true
		} else {
			sup.scrcpyFullyStarted = false
		}
		if ch != nil && n > 0 {
			for i := 0; i < n; i++ {
				ch <- m.err
			}
		}

	case msgWebRTCRelease:
		*webrtcN--
		if *webrtcN < 0 {
			logutil.Warnf("[streamSupervisor] %s: webrtcN 修正（<0）", sup.key)
			*webrtcN = 0
		}
		if sup.noConsumersLeft(webrtcN) {
			sup.teardownConsumersLocked(webrtcN)
		}

	case msgPeerClose:
		sup.removeOneWebRTCSession(m.sessionID, webrtcN)

	case msgRegisterVideoPeer:
		if !m.streamer.IsRunning() {
			m.reply <- fmt.Errorf("流未运行，无法添加视频轨道")
			return
		}
		sup.stopVideoPeer(m.sessionID)
		ch := make(chan *VideoFrame, supVideoPeerFrameCap)
		m.streamer.RegisterWebRTCVideoSub(m.sessionID, ch)
		h := NewH264Streamer(m.track)
		vp := &supVideoPeer{h264: h, peerConn: m.peerConn}
		sup.videoPeers[m.sessionID] = vp
		go runWebRTCVideoSinkLoop(sup.mgr, sup.key, m.sessionID, ch, h, m.peerConn)
		sup.bindScrcpyUpstream()
		m.reply <- nil
		logutil.Infof("[streamSupervisor] %s: 已登记视频轨（chan+独立 goroutine）session=%s（当前 %d 路）", sup.key, m.sessionID, len(sup.videoPeers))

	case msgWebRTCControl:
		if len(m.data) == 0 || m.data[0] == 0xFF {
			return
		}
		if v, ok := sup.mgr.streams.Load(sup.key); ok {
			if _, ok := v.(*sync.Map).Load(m.sessionID); !ok {
				return
			}
		} else {
			return
		}
		v, ok := sup.mgr.deviceStreamers.Load(sup.key)
		if !ok {
			return
		}
		st := v.(*AndroidStreamer)
		srv := st.GetScrcpyServer()
		if srv == nil {
			return
		}
		if err := srv.SendControlMessage(m.data); err != nil {
			if scrcpyControlSendIOFatal(err) {
				logutil.Debugf("[WebRTC] 设备 %s: scrcpy 控制写出错，supervisor 排队断开: %v", sup.key, err)
				// 勿在 run 内对 ctrl 做阻塞自投递（死锁）；异步投递保证最终入队
				go func() { sup.ctrl <- msgDeviceDisconnect{} }()
			}
		}

	case msgScrcpyDeviceBroadcast:
		if len(m.payload) == 0 {
			return
		}
		v, ok := sup.mgr.streams.Load(sup.key)
		if !ok {
			return
		}
		sm := v.(*sync.Map)
		sm.Range(func(_, si interface{}) bool {
			sess := si.(*StreamSession)
			if sess.DataChannel != nil && sess.DataChannel.ReadyState() == webrtc.DataChannelStateOpen {
				if err := sess.DataChannel.Send(m.payload); err != nil {
					logutil.Errorf("[WebRTC] 设备 %s 会话 %s: 下发设备消息失败: %v", sup.key, sess.sessionID, err)
				}
			}
			return true
		})

	case msgDeviceDisconnect:
		logutil.Debugf("[WebRTC] 设备 %s: supervisor 处理设备断开", sup.key)
		sup.abortScrcpyWaitBroadcast(errors.New("device disconnect"))
		sup.clearVideoPeers()
		if _, ok := sup.mgr.streams.Load(sup.key); ok {
			sup.mgr.closeAllSessionsPeerConns(sup.key, true)
			sup.mgr.streams.Delete(sup.key)
		}
		*webrtcN = 0
		if s, ok := sup.mgr.deviceStreamers.LoadAndDelete(sup.key); ok {
			ast := s.(*AndroidStreamer)
			ast.SetWebRTCVideoPipelineActive(false)
			ast.Stop()
			logutil.Debugf("[WebRTC] 设备 %s: scrcpy-server已停止（supervisor）", sup.key)
		}
		sup.retireSupervisor()
	}
}

func (sup *streamSupervisor) stopVideoPeer(sessionID string) {
	if v, ok := sup.mgr.deviceStreamers.Load(sup.key); ok {
		v.(*AndroidStreamer).RemoveWebRTCVideoSub(sessionID)
	}
	delete(sup.videoPeers, sessionID)
}

func (sup *streamSupervisor) clearVideoPeers() {
	ids := make([]string, 0, len(sup.videoPeers))
	for sid := range sup.videoPeers {
		ids = append(ids, sid)
	}
	for _, sid := range ids {
		sup.stopVideoPeer(sid)
	}
}

func shouldRemoveVideoPeer(err error, peerConn *webrtc.PeerConnection) bool {
	errStr := err.Error()
	if errStr == "track closed" || errStr == "track is closed" ||
		errStr == "connection closed" || errStr == "connection is closed" ||
		strings.Contains(errStr, "closed") {
		return true
	}
	if peerConn != nil {
		state := peerConn.ConnectionState()
		if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
			return true
		}
	}
	return false
}

func (sup *streamSupervisor) teardownConsumersLocked(webrtcN *int) {
	sup.abortScrcpyWaitBroadcast(errors.New("teardown consumers"))
	sup.clearVideoPeers()
	if s, ok := sup.mgr.deviceStreamers.LoadAndDelete(sup.key); ok {
		ast := s.(*AndroidStreamer)
		ast.SetWebRTCVideoPipelineActive(false)
		ast.Stop()
		logutil.Debugf("[WebRTC] 设备 %s: 无消费者，已停止 stream（supervisor）", sup.key)
	}
	sup.mgr.streams.Delete(sup.key)
	if webrtcN != nil {
		*webrtcN = 0
	}
	sup.retireSupervisor()
}

// retireSupervisor 设备侧已无投屏消费者：从全局表摘除，使 supervisorFor 下次新建；当前 run 在处理完本条 ctrl 后退出。
func (sup *streamSupervisor) retireSupervisor() {
	if sup.mgr.streamSupervisors.CompareAndDelete(sup.key, sup) {
		logutil.Debugf("[streamSupervisor] %s: 已从 streamSupervisors 摘除，本 supervisor 将退出", sup.key)
	}
	// 无论 CAS 是否成功（例如 map 已被新 sup 替换），本 goroutine 都必须退出，否则 retired=0 会卡死 run。
	atomic.StoreInt32(&sup.retired, 1)
}

func (sup *streamSupervisor) webRTCReserve(streamer *AndroidStreamer) msgWebRTCReserveReply {
	reply := make(chan msgWebRTCReserveReply, 1)
	sup.ctrl <- msgWebRTCReserve{streamer: streamer, reply: reply}
	return <-reply
}

// tryWebRTCRelease CreateStream 失败路径：须可靠递减 webrtcN。已 retire 则不必发；否则投递 Release，并与 run 退出对齐，避免 run 已停仍永久阻塞在 ctrl 上。
func (sup *streamSupervisor) tryWebRTCRelease() {
	if atomic.LoadInt32(&sup.retired) != 0 {
		return
	}
	rd := sup.runDone
	if rd == nil {
		sup.ctrl <- msgWebRTCRelease{}
		return
	}
	select {
	case sup.ctrl <- msgWebRTCRelease{}:
	case <-rd:
		if atomic.LoadInt32(&sup.retired) != 0 {
			return
		}
		logutil.Warnf("[streamSupervisor] %s: tryWebRTCRelease 时 supervisor run 已退出且未 retire，webrtcN 可能漂移", sup.key)
	}
}

func (sup *streamSupervisor) postPeerClose(sessionID string) {
	sup.ctrl <- msgPeerClose{sessionID: sessionID}
}

func (sup *streamSupervisor) postWebRTCControl(sessionID string, data []byte) {
	sup.ctrl <- msgWebRTCControl{sessionID: sessionID, data: data}
}

func (sup *streamSupervisor) registerVideoPeer(st *AndroidStreamer, sessionID string, track *webrtc.TrackLocalStaticSample, pc *webrtc.PeerConnection) error {
	reply := make(chan error, 1)
	sup.ctrl <- msgRegisterVideoPeer{streamer: st, sessionID: sessionID, track: track, peerConn: pc, reply: reply}
	return <-reply
}

func (sup *streamSupervisor) bindScrcpyUpstream() {
	v, ok := sup.mgr.deviceStreamers.Load(sup.key)
	if !ok {
		return
	}
	st := v.(*AndroidStreamer)
	srv := st.GetScrcpyServer()
	if srv == nil {
		return
	}
	srv.SetDeviceMessageHandler(func(msg []byte) {
		if len(msg) == 0 {
			return
		}
		p := make([]byte, len(msg))
		copy(p, msg)
		select {
		case sup.ctrl <- msgScrcpyDeviceBroadcast{payload: p}:
		default:
			logutil.Warnf("[streamSupervisor] %s: ctrl 满，丢弃设备上行 %dB", sup.key, len(p))
		}
	})
}

// runWebRTCVideoSinkLoop 每会话一条 goroutine：只从 scrcpy 转发来的 chan 读帧并写 WebRTC；与 scrcpy 无直接引用。
func runWebRTCVideoSinkLoop(m *WebRTCManager, streamKey, sessionID string, ch <-chan *VideoFrame, h *H264Streamer, pc *webrtc.PeerConnection) {
	for f := range ch {
		if f == nil || len(f.Data) == 0 {
			continue
		}
		err := h.ProcessH264Frame(f.Data, f.IsConfig, f.IsKeyFrame)
		if err != nil && shouldRemoveVideoPeer(err, pc) {
			m.removeStreamSession(streamKey, sessionID)
			return
		}
	}
}

func scrcpyControlSendIOFatal(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "wsasend: An established connection was aborted by the software in your host machine.") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "EOF")
}
