package streaming

import (
	"fmt"
	"github.com/ms-robots/ms-robot/internal/logutil"
	"net"
	"sync"

	"github.com/pion/turn/v2"
)

// TurnServer TURN服务器管理器
type TurnServer struct {
	udpListener net.PacketConn
	tcpListener net.Listener
	server      *turn.Server
	port        int
	realm       string
	username    string
	password    string
	mu          sync.RWMutex
	running     bool
}

// NewTurnServer 创建TURN服务器
func NewTurnServer(port int, realm, username, password string) (*TurnServer, error) {
	ts := &TurnServer{
		port:     port,
		realm:    realm,
		username: username,
		password: password,
	}

	// 创建UDP监听器
	udpAddr := fmt.Sprintf("0.0.0.0:%d", port)
	udpListener, err := net.ListenPacket("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("创建UDP监听器失败: %v", err)
	}
	ts.udpListener = udpListener

	// 创建TCP监听器
	tcpAddr := fmt.Sprintf("0.0.0.0:%d", port)
	tcpListener, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		udpListener.Close()
		return nil, fmt.Errorf("创建TCP监听器失败: %v", err)
	}
	ts.tcpListener = tcpListener

	// 创建TURN服务器配置
	// 使用 PacketConnConfigs 用于 UDP
	serverConfig := turn.ServerConfig{
		PacketConnConfigs: []turn.PacketConnConfig{
			{
				PacketConn: udpListener,
				RelayAddressGenerator: &turn.RelayAddressGeneratorNone{
					Address: "0.0.0.0",
				},
			},
		},
		Realm: realm,
		AuthHandler: func(username string, realm string, srcAddr net.Addr) ([]byte, bool) {
			// 简单的用户名密码验证
			if username == ts.username && realm == ts.realm {
				key := turn.GenerateAuthKey(username, realm, ts.password)
				return key, true
			}
			return nil, false
		},
	}

	// 创建TURN服务器
	server, err := turn.NewServer(serverConfig)
	if err != nil {
		udpListener.Close()
		tcpListener.Close()
		return nil, fmt.Errorf("创建TURN服务器失败: %v", err)
	}
	ts.server = server

	logutil.Infof("[TURN] TURN服务器已创建，监听端口: %d (UDP/TCP)", port)
	return ts, nil
}

// Start 启动TURN服务器
func (ts *TurnServer) Start() error {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.running {
		return fmt.Errorf("TURN服务器已在运行")
	}

	// 在goroutine中处理TCP连接
	go func() {
		for {
			conn, err := ts.tcpListener.Accept()
			if err != nil {
				if ts.running {
					logutil.Errorf("[TURN] TCP接受连接失败: %v", err)
				}
				return
			}

			// 处理TCP连接（这里简化处理，实际应该创建TCP TURN连接）
			go func() {
				defer conn.Close()
				// TODO: 实现TCP TURN连接处理
				logutil.Debugf("[TURN] 收到TCP连接: %s", conn.RemoteAddr())
			}()
		}
	}()

	ts.running = true
	logutil.Infof("[TURN] TURN服务器已启动，监听端口: %d", ts.port)
	return nil
}

// Stop 停止TURN服务器
func (ts *TurnServer) Stop() error {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if !ts.running {
		return nil
	}

	ts.running = false

	if ts.server != nil {
		ts.server.Close()
	}
	if ts.udpListener != nil {
		ts.udpListener.Close()
	}
	if ts.tcpListener != nil {
		ts.tcpListener.Close()
	}

	logutil.Infof("[TURN] TURN服务器已停止")
	return nil
}

// GetPort 获取监听端口
func (ts *TurnServer) GetPort() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.port
}

// GetURL 获取TURN服务器URL（用于ICE配置）
func (ts *TurnServer) GetURL(publicIP string) string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if publicIP == "" {
		publicIP = "127.0.0.1"
	}
	return fmt.Sprintf("turn:%s:%d", publicIP, ts.port)
}

// GetUsername 获取用户名
func (ts *TurnServer) GetUsername() string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.username
}

// GetPassword 获取密码
func (ts *TurnServer) GetPassword() string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.password
}
