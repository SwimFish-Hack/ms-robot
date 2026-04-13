package websocket

import (
	"encoding/json"
	"github.com/ms-robots/ms-robot/internal/device"
	"sync"

	"github.com/gorilla/websocket"
)

// MessageType 消息类型（统一约定：所有推送均为 type + data，必要时带 device_udid）
//
// 当前事件：
//   - device_status: 单台设备状态变化（上线/下线等），Data 为设备对象，带 device_udid
//   - endpoint_status: 单个端点连接状态（ok/reconnecting/reconnect_failed/removed），Data 为 EndpointStatusPayload
//   - endpoints_changed: 端点列表已变更（增删），前端应刷新端点与设备列表，Data 可为 nil
//   - error: 预留，暂未使用
type MessageType string

const (
	MessageDeviceStatus     MessageType = "device_status"
	MessageEndpointStatus   MessageType = "endpoint_status"
	MessageEndpointsChanged MessageType = "endpoints_changed"
	MessageError            MessageType = "error"
)

// Message WebSocket消息
type Message struct {
	Type       MessageType `json:"type"`
	DeviceUDID string      `json:"device_udid,omitempty"`
	Data       interface{} `json:"data"`
}

// EndpointStatusPayload 端点状态推送
type EndpointStatusPayload struct {
	Endpoint    string `json:"endpoint"`     // 端点 key
	DisplayName string `json:"display_name"` // 显示名（用于前端匹配 section）
	Status      string `json:"status"`       // ok | reconnecting | reconnect_failed | removed
}

// Client WebSocket客户端连接
type Client struct {
	hub        *Hub
	conn       *websocket.Conn
	send       chan []byte
	deviceUDID string // 客户端关注的设备（可选）
}

// Hub WebSocket Hub
type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	mu         sync.RWMutex
}

// NewHub 创建WebSocket Hub
func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

// Run 运行Hub
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			h.mu.Unlock()

		case message := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(h.clients, client)
				}
			}
			h.mu.RUnlock()
		}
	}
}

// RegisterClient 注册客户端
func (h *Hub) RegisterClient(conn *websocket.Conn, deviceUDID string) *Client {
	client := &Client{
		hub:        h,
		conn:       conn,
		send:       make(chan []byte, 256),
		deviceUDID: deviceUDID,
	}

	h.register <- client

	go client.writePump()
	go client.readPump()

	return client
}

// BroadcastDeviceStatus 广播设备状态
func (h *Hub) BroadcastDeviceStatus(device *device.Device) {
	message := Message{
		Type:       MessageDeviceStatus,
		DeviceUDID: device.UDID,
		Data:       device,
	}

	data, _ := json.Marshal(message)
	h.broadcast <- data
}

// BroadcastEndpointStatus 广播端点连接状态（重连中/重连失败/已移除）
func (h *Hub) BroadcastEndpointStatus(endpointKey, displayName, status string) {
	message := Message{
		Type: MessageEndpointStatus,
		Data: EndpointStatusPayload{
			Endpoint:    endpointKey,
			DisplayName: displayName,
			Status:      status,
		},
	}
	data, _ := json.Marshal(message)
	h.broadcast <- data
}

// BroadcastEndpointsChanged 广播端点列表已变更（增删端点），前端应刷新端点与设备列表
func (h *Hub) BroadcastEndpointsChanged() {
	message := Message{Type: MessageEndpointsChanged, Data: nil}
	data, _ := json.Marshal(message)
	h.broadcast <- data
}

// readPump 读取客户端消息
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()

	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				// 日志错误
			}
			break
		}
	}
}

// writePump 向客户端发送消息
func (c *Client) writePump() {
	defer c.conn.Close()

	for message := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
			return
		}
	}
	_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
}
