package adb

import (
	"fmt"

	"github.com/ms-robots/ms-robot/internal/logutil"
	"github.com/ms-robots/ms-robot/third_party/adb/internal/errors"
	"github.com/ms-robots/ms-robot/third_party/adb/wire"
)

const (
	// Default port the adb server listens on.
	AdbPort = 5037
)

type ServerConfig struct {
	// Host and port the adb server is listening on.
	// Default: Host "127.0.0.1", Port 5037. Can point to a remote machine.
	Host string
	Port int

	// Dialer used to connect to the adb server.
	Dialer
}

// Server knows how to connect to an adb server (no exec; server must already be running).
type server interface {
	Start() error
	Dial() (*wire.Conn, error)
}

// panic 成因说明：若 Dial() 返回 (nil, nil)（底层如 proxy 超时/异常时可能发生），
// 未校验 conn 时会执行 defer conn.Close() 与 conn.RoundTripSingleResponse(conn)，
// 对 nil 解引用导致 panic。故此处必须对 conn == nil 做校验并返回错误，避免 crash。
func roundTripSingleResponse(s server, req string) ([]byte, error) {
	logutil.Debugf("[adb] roundTrip 开始 %s", req)
	conn, err := s.Dial()
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, errors.WrapErrorf(nil, errors.ServerNotAvailable, "Dial 返回空连接")
	}
	defer conn.Close()

	logutil.Debugf("[adb] >> %s", req)
	resp, err := conn.RoundTripSingleResponse([]byte(req))
	if err != nil {
		logutil.Debugf("[adb] << err: %v", err)
		logutil.Warnf("[adb] 读响应失败原因: %s", errors.ErrorWithCauseChain(err))
		return nil, err
	}
	logutil.Debugf("[adb] << %d bytes", len(resp))
	return resp, nil
}

func roundTripSingleNoResponse(s server, req string) error {
	conn, err := s.Dial()
	if err != nil {
		return err
	}
	if conn == nil {
		return errors.WrapErrorf(nil, errors.ServerNotAvailable, "Dial 返回空连接")
	}
	defer conn.Close()
	return conn.RoundTripSingleNoResponse([]byte(req))
}

// roundTripTransportThenNoResponse sends two requests on one connection: first
// host:transport:<serial> (switch to device), then the payload (e.g. reverse:forward:...).
// Matches official adb client behavior for device-local services like reverse.
func roundTripTransportThenNoResponse(s server, transportReq, payloadReq string) error {
	conn, err := s.Dial()
	if err != nil {
		return err
	}
	if conn == nil {
		return errors.WrapErrorf(nil, errors.ServerNotAvailable, "Dial 返回空连接")
	}
	defer conn.Close()
	if err := conn.SendMessage([]byte(transportReq)); err != nil {
		return err
	}
	if _, err := conn.ReadStatus(transportReq); err != nil {
		return err
	}
	return conn.RoundTripSingleNoResponse([]byte(payloadReq))
}

type realServer struct {
	config  ServerConfig
	address string
}

func newServer(config ServerConfig) (server, error) {
	if config.Dialer == nil {
		config.Dialer = tcpDialer{}
	}

	if config.Host == "" {
		config.Host = "127.0.0.1"
	}
	if config.Port == 0 {
		config.Port = AdbPort
	}

	return &realServer{
		config:  config,
		address: fmt.Sprintf("%s:%d", config.Host, config.Port),
	}, nil
}

// Dial connects to the adb server. No exec; the server must already be running (local or remote).
// 若 Dialer 在极端情况（如超时/代理库）下返回 (nil, nil)，此处校验避免后续对 nil conn 解引用导致 panic。
func (s *realServer) Dial() (*wire.Conn, error) {
	logutil.Debugf("[adb] Dial %s", s.address)
	conn, err := s.config.Dial(s.address)
	if err != nil {
		logutil.Warnf("[adb] Dial 失败: %v", err)
		return nil, err
	}
	if conn == nil {
		return nil, errors.WrapErrorf(nil, errors.ServerNotAvailable, "Dial 返回空连接")
	}
	return conn, nil
}

// Start is not supported: this client does not exec adb. The adb server must already be running.
func (s *realServer) Start() error {
	return errors.WrapErrorf(nil, errors.ServerNotAvailable, "adb server must already be running (this client does not exec adb)")
}
