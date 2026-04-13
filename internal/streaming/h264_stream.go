package streaming

import (
	"bytes"
	"github.com/ms-robots/ms-robot/internal/logutil"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

// H264Streamer H.264视频流处理器
type H264Streamer struct {
	videoTrack        *webrtc.TrackLocalStaticSample
	sps               []byte // Sequence Parameter Set
	pps               []byte // Picture Parameter Set
	gotConfig         bool
	frameCount        int       // 帧计数器
	lastPTS           uint64    // 上一个PTS，用于计算时长
	lastKeyFrameCount int       // 上次关键帧的帧序号
	lastKeyFrameTime  time.Time // 上次关键帧的时间
	// 帧率跟踪
	ptsDeltas        []uint64      // 最近N帧的PTS差值，用于计算平均帧率
	avgFrameDuration time.Duration // 平均帧时长（根据实际PTS计算）
	mu               sync.Mutex    // 保护关键帧请求状态
}

// NewH264Streamer 创建H.264流处理器
func NewH264Streamer(videoTrack *webrtc.TrackLocalStaticSample) *H264Streamer {
	return &H264Streamer{
		videoTrack: videoTrack,
	}
}

// ProcessH264Frame 处理H.264帧
// scrcpy 发送的数据已经是 Annex-B 格式（带起始码），直接发送到 WebRTC 即可
func (h *H264Streamer) ProcessH264Frame(frame []byte, isConfig bool, isKeyFrame bool) error {
	if len(frame) == 0 {
		return nil
	}

	// 提取PTS信息（前8字节，如果存在）
	var pts uint64
	var frameData []byte
	if len(frame) >= 8 {
		// 提取PTS（big-endian）
		pts = uint64(frame[0])<<56 | uint64(frame[1])<<48 |
			uint64(frame[2])<<40 | uint64(frame[3])<<32 |
			uint64(frame[4])<<24 | uint64(frame[5])<<16 |
			uint64(frame[6])<<8 | uint64(frame[7])
		frameData = frame[8:]
	} else {
		// 没有PTS信息，使用默认值
		frameData = frame
		pts = 0
	}

	// 计算帧时长（使用实际PTS差值，并跟踪平均帧率）
	var duration time.Duration
	if h.lastPTS > 0 && pts > h.lastPTS {
		// 根据PTS差值计算时长（PTS单位是微秒）
		deltaPTS := pts - h.lastPTS
		duration = time.Duration(deltaPTS) * time.Microsecond
		// 限制 duration：乘法溢出、PTS 大跳变、过短间隔
		switch {
		case duration <= 0:
			h.mu.Lock()
			if h.avgFrameDuration > 0 {
				duration = h.avgFrameDuration
			} else {
				duration = time.Millisecond * 33
			}
			h.mu.Unlock()
		case duration > 1*time.Second:
			h.mu.Lock()
			if h.avgFrameDuration > 0 {
				duration = h.avgFrameDuration
			} else {
				duration = 1 * time.Second
			}
			h.mu.Unlock()
		case duration < 1*time.Millisecond:
			duration = 1 * time.Millisecond
		}

		// 更新帧率统计（收集最近15帧的PTS差值，用于计算平均帧率，减少计算开销）
		h.mu.Lock()
		if h.ptsDeltas == nil {
			h.ptsDeltas = make([]uint64, 0, 15)
		}
		// 添加当前帧的PTS差值
		h.ptsDeltas = append(h.ptsDeltas, deltaPTS)
		// 只保留最近15帧（减少内存和计算开销）
		if len(h.ptsDeltas) > 15 {
			h.ptsDeltas = h.ptsDeltas[len(h.ptsDeltas)-15:]
		}
		// 计算平均PTS差值（至少3帧才开始计算，减少计算频率）
		if len(h.ptsDeltas) >= 3 {
			var sum uint64
			for _, d := range h.ptsDeltas {
				sum += d
			}
			avgDeltaPTS := sum / uint64(len(h.ptsDeltas))
			h.avgFrameDuration = time.Duration(avgDeltaPTS) * time.Microsecond
			// 限制平均帧时长范围
			if h.avgFrameDuration < 1*time.Millisecond {
				h.avgFrameDuration = 1 * time.Millisecond
			} else if h.avgFrameDuration > 1*time.Second {
				h.avgFrameDuration = 1 * time.Second
			}
		}
		h.mu.Unlock()
	} else {
		// 第一帧或PTS无效，使用平均帧率或默认值
		h.mu.Lock()
		if h.avgFrameDuration > 0 {
			duration = h.avgFrameDuration
		} else {
			duration = time.Millisecond * 33 // 30fps 默认值
		}
		h.mu.Unlock()

		// 静默处理第一帧和PTS无效的情况，不打印日志
	}
	h.lastPTS = pts
	h.frameCount++

	// 如果是配置帧，提取SPS/PPS并保存
	if isConfig {
		// 配置帧通常只包含SPS和PPS，但为了安全起见，我们也发送原始配置帧数据
		// 这样即使提取失败，WebRTC也能收到配置信息

		// 解析NAL单元，提取SPS和PPS
		i := 0
		for i < len(frameData)-3 {
			// 查找起始码
			if frameData[i] == 0x00 && frameData[i+1] == 0x00 {
				var nalStart int
				if i+3 < len(frameData) && frameData[i+2] == 0x00 && frameData[i+3] == 0x01 {
					// 4字节起始码: 00 00 00 01
					nalStart = i + 4
				} else if i+2 < len(frameData) && frameData[i+2] == 0x01 {
					// 3字节起始码: 00 00 01
					nalStart = i + 3
				} else {
					i++
					continue
				}

				if nalStart >= len(frameData) {
					break
				}

				// 检查NAL类型
				nalType := frameData[nalStart] & 0x1F

				// 找到下一个起始码或数据结束
				nalEnd := len(frameData)
				for j := nalStart + 1; j < len(frameData)-3; j++ {
					if frameData[j] == 0x00 && frameData[j+1] == 0x00 {
						if j+3 < len(frameData) && frameData[j+2] == 0x00 && frameData[j+3] == 0x01 {
							nalEnd = j
							break
						} else if j+2 < len(frameData) && frameData[j+2] == 0x01 {
							nalEnd = j
							break
						}
					}
				}

				nalData := frameData[nalStart:nalEnd]
				switch nalType {
				case 7: // SPS
					h.sps = make([]byte, len(nalData))
					copy(h.sps, nalData)
				case 8: // PPS
					h.pps = make([]byte, len(nalData))
					copy(h.pps, nalData)
				}

				// 移动到下一个NAL单元
				i = nalEnd
			} else {
				i++
			}
		}

		// 发送配置帧到WebRTC
		// 优先使用提取的SPS/PPS打包的配置帧，如果没有提取到，则发送原始配置帧数据
		var configDataToSend []byte
		if len(h.sps) > 0 && len(h.pps) > 0 {
			configDataToSend = h.packConfig()
			// 使用提取的SPS/PPS打包配置帧（不打印详细日志）
		} else {
			// 如果提取失败，发送原始配置帧数据
			configDataToSend = frameData
			logutil.Warnf("[H264Streamer] ⚠️ 未提取到SPS/PPS，发送原始配置帧数据 (%d字节)", len(configDataToSend))
		}

		if err := h.videoTrack.WriteSample(media.Sample{
			Data:     configDataToSend,
			Duration: 0, // 配置帧无时长
		}); err != nil {
			logutil.Errorf("[H264Streamer] 发送配置帧失败: %v", err)
			return err
		}

		if len(h.sps) > 0 && len(h.pps) > 0 {
			h.gotConfig = true
		}
		// 配置帧已发送（不打印详细日志）

		// 配置帧处理完成，不需要再作为普通帧发送
		return nil
	}

	// 如果还没有配置，尝试从关键帧中提取SPS/PPS
	if !h.gotConfig && isKeyFrame {
		// 收到关键帧但尚未有配置，尝试提取SPS/PPS（不打印详细日志）
		// 从关键帧中提取SPS/PPS（关键帧可能包含SPS/PPS）
		i := 0
		for i < len(frameData)-3 {
			if frameData[i] == 0x00 && frameData[i+1] == 0x00 {
				var nalStart int
				if i+3 < len(frameData) && frameData[i+2] == 0x00 && frameData[i+3] == 0x01 {
					nalStart = i + 4
				} else if i+2 < len(frameData) && frameData[i+2] == 0x01 {
					nalStart = i + 3
				} else {
					i++
					continue
				}

				if nalStart >= len(frameData) {
					break
				}

				nalType := frameData[nalStart] & 0x1F
				nalEnd := len(frameData)
				for j := nalStart + 1; j < len(frameData)-3; j++ {
					if frameData[j] == 0x00 && frameData[j+1] == 0x00 {
						if j+3 < len(frameData) && frameData[j+2] == 0x00 && frameData[j+3] == 0x01 {
							nalEnd = j
							break
						} else if j+2 < len(frameData) && frameData[j+2] == 0x01 {
							nalEnd = j
							break
						}
					}
				}

				nalData := frameData[nalStart:nalEnd]
				switch nalType {
				case 7: // SPS
					h.sps = make([]byte, len(nalData))
					copy(h.sps, nalData)
					logutil.Infof("[H264Streamer] 从关键帧提取SPS: %d 字节", len(h.sps))
				case 8: // PPS
					h.pps = make([]byte, len(nalData))
					copy(h.pps, nalData)
					logutil.Infof("[H264Streamer] 从关键帧提取PPS: %d 字节", len(h.pps))
				}

				i = nalEnd
			} else {
				i++
			}
		}

		// 如果提取到SPS和PPS，发送配置帧
		if len(h.sps) > 0 && len(h.pps) > 0 {
			configData := h.packConfig()
			if err := h.videoTrack.WriteSample(media.Sample{
				Data:     configData,
				Duration: 0,
			}); err != nil {
				logutil.Errorf("[H264Streamer] 发送配置帧失败: %v", err)
			} else {
				// 已发送SPS/PPS配置帧到WebRTC（从关键帧提取，不打印详细日志）
				h.gotConfig = true
			}
		}
	}

	// 检查是否是关键帧（如果参数未提供，则通过NAL类型检查）
	if !isKeyFrame {
		for i := 0; i < len(frameData)-4; i++ {
			if frameData[i] == 0x00 && frameData[i+1] == 0x00 &&
				(frameData[i+2] == 0x00 && frameData[i+3] == 0x01 || frameData[i+2] == 0x01) {
				nalStart := i + 4
				if frameData[i+2] == 0x01 {
					nalStart = i + 3
				}
				if nalStart < len(frameData) && frameData[nalStart]&0x1F == 5 { // IDR
					isKeyFrame = true
					break
				}
			}
		}
	}

	// 为提升浏览器解码恢复率：关键帧前重发一份缓存的 SPS/PPS。
	if isKeyFrame && len(h.sps) > 0 && len(h.pps) > 0 {
		configData := h.packConfig()
		if err := h.videoTrack.WriteSample(media.Sample{
			Data:     configData,
			Duration: 0,
		}); err != nil {
			logutil.Errorf("[H264Streamer] 关键帧前重发配置失败: %v", err)
			return err
		}
	}

	// 发送到WebRTC轨道
	if err := h.videoTrack.WriteSample(media.Sample{
		Data:     frameData,
		Duration: duration,
	}); err != nil {
		// 只在错误时解析NAL类型（减少正常流程的开销）
		errStr := err.Error()
		if errStr == "track closed" || errStr == "track is closed" {
			// 连接关闭，简单记录即可
			return err
		}
		// 其他错误才详细记录（减少日志开销）
		logutil.Errorf("[H264Streamer] 发送帧失败: %v (帧大小: %d, 是否关键帧: %t, 帧序号: %d)",
			err, len(frameData), isKeyFrame, h.frameCount)
		return err
	}

	// 更新关键帧统计（只在关键帧时更新，减少锁竞争）
	if isKeyFrame {
		h.mu.Lock()
		h.lastKeyFrameCount = h.frameCount
		h.lastKeyFrameTime = time.Now()
		h.mu.Unlock()
	}

	return nil
}

// packConfig 打包SPS/PPS配置
// WebRTC H.264 配置格式：使用 Annex-B 格式（带起始码）
func (h *H264Streamer) packConfig() []byte {
	var buf bytes.Buffer

	// 使用 Annex-B 格式：每个 NAL 单元前加起始码 0x00 0x00 0x00 0x01
	startCode := []byte{0x00, 0x00, 0x00, 0x01}

	// 写入 SPS（如果存在）
	if len(h.sps) > 0 {
		buf.Write(startCode)
		buf.Write(h.sps)
	}

	// 写入 PPS（如果存在）
	if len(h.pps) > 0 {
		buf.Write(startCode)
		buf.Write(h.pps)
	}

	return buf.Bytes()
}
