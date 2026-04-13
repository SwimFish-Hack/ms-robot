package adb

import (
	"net"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	"github.com/ms-robots/ms-robot/third_party/adb/internal/errors"
	"github.com/ms-robots/ms-robot/third_party/adb/wire"
)

const defaultDialTimeout = 20 * time.Second

// SOCKS5Dialer 通过 SOCKS5 代理连接 ADB 服务器
type SOCKS5Dialer struct {
	proxyAddr string
	auth      *proxy.Auth
	dialer    proxy.Dialer
}

// NewSOCKS5Dialer 创建 SOCKS5 Dialer。proxyURL 格式：socks5://host、socks5://host:port、socks5://user:pass@host:port；未写 port 时默认 1080。
func NewSOCKS5Dialer(proxyURL string) (*SOCKS5Dialer, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL != "" && !strings.Contains(proxyURL, "://") {
		proxyURL = "socks5://" + proxyURL
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, errors.WrapErrorf(err, errors.ServerNotAvailable, "解析 proxy URL 失败: %v", err)
	}
	if u.Scheme != "socks5" {
		return nil, errors.WrapErrorf(nil, errors.ServerNotAvailable, "proxy 只支持 socks5，需要 socks5://，当前: %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, errors.WrapErrorf(nil, errors.ServerNotAvailable, "proxy URL 缺少 host: %s", proxyURL)
	}
	port := u.Port()
	if port == "" {
		port = "1080"
	}
	proxyAddr := net.JoinHostPort(u.Hostname(), port)

	// username/password 会传给 proxy.SOCKS5，连接代理时用于 SOCKS5 用户名密码认证（RFC 1929）
	var auth *proxy.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = &proxy.Auth{
			User:     u.User.Username(),
			Password: pass,
		}
	}

	// 创建 SOCKS5 dialer
	dialer, err := proxy.SOCKS5("tcp", proxyAddr, auth, proxy.Direct)
	if err != nil {
		return nil, errors.WrapErrorf(err, errors.ServerNotAvailable, "创建 SOCKS5 拨号器失败 (代理: %s): %v", proxyAddr, err)
	}

	return &SOCKS5Dialer{
		proxyAddr: proxyAddr,
		auth:      auth,
		dialer:    dialer,
	}, nil
}

// Dial 实现 adb.Dialer 接口，通过 SOCKS5 代理连接目标地址（带建连超时）。
func (d *SOCKS5Dialer) Dial(address string) (*wire.Conn, error) {
	netConn, err := d.dialWithTimeout(address)
	if err != nil {
		return nil, err
	}
	if netConn == nil {
		return nil, errors.WrapErrorf(nil, errors.ServerNotAvailable, "dial returned nil connection")
	}
	safeConn := wire.MultiCloseable(netConn)
	return &wire.Conn{
		Scanner: wire.NewScanner(safeConn),
		Sender:  wire.NewSender(safeConn),
	}, nil
}

func (d *SOCKS5Dialer) dialWithTimeout(address string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := d.dialer.Dial("tcp", address)
		done <- result{conn, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			return nil, errors.WrapErrorf(res.err, errors.ServerNotAvailable, "通过 SOCKS5 代理 %s 连接 %s 失败: %v", d.proxyAddr, address, res.err)
		}
		if res.conn == nil {
			return nil, errors.WrapErrorf(nil, errors.ServerNotAvailable, "通过 SOCKS5 代理连接 %s 返回空连接", address)
		}
		return res.conn, nil
	case <-time.After(defaultDialTimeout):
		return nil, errors.WrapErrorf(nil, errors.ServerNotAvailable, "通过 SOCKS5 代理连接 %s 超时", address)
	}
}

// DialTCP 直接返回 net.Conn（用于 Forward 端口等普通 TCP 连接）。
func (d *SOCKS5Dialer) DialTCP(address string) (net.Conn, error) {
	return d.dialWithTimeout(address)
}
