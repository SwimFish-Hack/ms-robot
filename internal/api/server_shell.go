package api

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// shell 终端 resize 消息：BinaryMessage，首字节 0x01，随后 4 字节 cols + 4 字节 rows（大端）
const shellResizeMsgType = 0x01

// handleShellWebSocket 建立 WebSocket 后通过 OpenCommand 在设备上启动交互式 shell，双向转发。
func (s *Server) handleShellWebSocket(c *gin.Context) {
	ep, deviceKey, ok := GetResolvedDevice(c)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少设备解析结果"})
		return
	}
	_, exists := s.getActiveDeviceResolved(ep, deviceKey)
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备不存在"})
		return
	}
	wsConn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer wsConn.Close()

	adbDevice, err := s.unifiedDeviceManager.GetADBDeviceInEndpoint(ep, deviceKey)
	if err != nil || adbDevice == nil {
		_ = wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "获取ADB设备失败: "+err.Error()))
		return
	}
	// 默认 80x24，启动不干预；仅拖窗口释放后发 resize 时注入 stty
	shellConn, err := adbDevice.OpenInteractiveShell()
	if err != nil {
		_ = wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "打开 shell 失败: "+err.Error()))
		return
	}
	defer shellConn.Close()

	done := make(chan struct{})
	var closeOnce sync.Once
	closeDone := func() { closeOnce.Do(func() { close(done) }) }

	// WebSocket -> 设备 shell stdin；BinaryMessage 0x01 为 resize，注入 stty（仅拖窗口释放后发一次）
	go func() {
		defer closeDone()
		for {
			mt, data, err := wsConn.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.CloseMessage {
				return
			}
			if mt == websocket.BinaryMessage && len(data) >= 9 && data[0] == shellResizeMsgType {
				cols := binary.BigEndian.Uint32(data[1:5])
				rows := binary.BigEndian.Uint32(data[5:9])
				if cols < 1 {
					cols = 1
				}
				if rows < 1 {
					rows = 1
				}
				if cols > 10000 {
					cols = 10000
				}
				if rows > 10000 {
					rows = 10000
				}
				stty := []byte(fmt.Sprintf("stty rows %d cols %d\n", rows, cols))
				if _, err := shellConn.Write(stty); err != nil {
					return
				}
				continue
			}
			if _, err := shellConn.Write(data); err != nil {
				return
			}
		}
	}()

	// 设备 shell stdout/stderr -> WebSocket：原样转发（保留 \r 供 ls 等列对齐），先复制再发送避免 buf 被覆盖
	go func() {
		defer closeDone()
		buf := make([]byte, 4096)
		for {
			n, err := shellConn.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if e := wsConn.WriteMessage(websocket.TextMessage, chunk); e != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					_ = wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error()))
				}
				return
			}
		}
	}()

	<-done
	_ = wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, ""))
}
