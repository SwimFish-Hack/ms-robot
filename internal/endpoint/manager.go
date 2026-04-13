package endpoint

import (
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/ms-robots/ms-robot/internal/logutil"
	adb "github.com/ms-robots/ms-robot/third_party/adb"
)

// DialTCPFunc 用于连接 adb 主机上的端口（如 forward 端口），走 proxy 时与 adb 命令同路
type DialTCPFunc func(addr string) (net.Conn, error)

const hexChars = "0123456789abcdef"

func randomHexID(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = hexChars[rand.Intn(len(hexChars))]
	}
	return string(b)
}

// 端点存储与标识（统一约定）
//
//  1. 内部 key = id（唯一标识，6 位 hex 随机生成）。name= 为备注，不检测重复，仅用于显示；未设 name 时显示 id。
//  2. 输入与解析：格式 key=value 逗号分隔。adb= 必填，name=、proxy=、retry= 可选。端点和 proxy 不能同时完全一样。
//  3. 删除：入参可为 id 或 host:port。

// Manager 端点管理器
type Manager struct {
	endpoints []string                      // 顺序列表，元素为 id（key）
	clients   map[string]*adb.Adb           // key(id) -> ADB 客户端
	hosts     map[string]string             // key -> host
	ports     map[string]int                // key -> port
	names     map[string]string             // key -> 显示名（name，可选）
	proxies   map[string]*SOCKS5ProxyConfig // key -> 该端点的 SOCKS5 代理（可选）
	retries   map[string]int                // key -> 断线重试次数
	mu        sync.RWMutex
}

// SOCKS5ProxyConfig SOCKS5 代理配置
type SOCKS5ProxyConfig struct {
	URL    string // socks5://host:port 或 socks5://user:pass@host:port
	Dialer *adb.SOCKS5Dialer
}

// NewManager 创建端点管理器
func NewManager() *Manager {
	return &Manager{
		endpoints: make([]string, 0),
		clients:   make(map[string]*adb.Adb),
		hosts:     make(map[string]string),
		ports:     make(map[string]int),
		names:     make(map[string]string),
		proxies:   make(map[string]*SOCKS5ProxyConfig),
		retries:   make(map[string]int),
	}
}

// parseEndpointSpec 解析 -endpoint 完整串：key=value 逗号分隔。adb= 必填，name= 为备注，retry 不填默认 -1。
func parseEndpointSpec(spec string) (adbValue, proxyURL, name string, retry int, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", "", "", 0, fmt.Errorf("端点不能为空")
	}
	retry = -1
	parts := strings.Split(spec, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		idx := strings.Index(part, "=")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(strings.ToLower(part[:idx]))
		val := strings.TrimSpace(part[idx+1:])
		switch key {
		case "adb":
			adbValue = val
		case "name":
			name = val
		case "proxy":
			proxyURL = val
		case "retry":
			if val != "" {
				if n, e := strconv.Atoi(val); e == nil {
					retry = n
				}
			}
		}
	}
	if adbValue == "" {
		return "", "", "", 0, fmt.Errorf("缺少 adb=，格式示例：adb=localhost:5037,name=本机,retry=5")
	}
	return adbValue, proxyURL, name, retry, nil
}

func (m *Manager) getProxyURL(key string) string {
	if p, ok := m.proxies[key]; ok && p != nil {
		return p.URL
	}
	return ""
}

// GetDialTCP 返回该端点用于连接 adb 主机端口的拨号函数（走 proxy 时与 adb 同路）；无 proxy 时返回 nil，调用方用 net.Dial。
func (m *Manager) GetDialTCP(key string) DialTCPFunc {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p, ok := m.proxies[key]; ok && p != nil && p.Dialer != nil {
		return p.Dialer.DialTCP
	}
	return nil
}

// AddEndpoint 添加端点。spec 格式：adb=host,name=xxx,proxy=...,retry=N。id 自动生成 6 位不重复，name 为备注不检重。silent 为 true 时不打「端点已添加」日志。
func (m *Manager) AddEndpoint(spec string, silent bool) (string, error) {
	adbValue, proxyURL, name, retry, err := parseEndpointSpec(spec)
	if err != nil {
		return "", fmt.Errorf("解析端点失败: %v", err)
	}
	host, port, err := parseEndpoint(adbValue)
	if err != nil {
		return "", fmt.Errorf("解析端点失败: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	const idLen = 6
	const maxAttempts = 100
	id := ""
	for i := 0; i < maxAttempts; i++ {
		candidate := randomHexID(idLen)
		if _, exists := m.clients[candidate]; !exists {
			id = candidate
			break
		}
	}
	if id == "" {
		return "", fmt.Errorf("随机 id 多次冲突")
	}
	// 端点和 proxy 两个不能同时完全一样
	for _, ep := range m.endpoints {
		if m.hosts[ep] == host && m.ports[ep] == port && m.getProxyURL(ep) == proxyURL {
			return "", fmt.Errorf("端点已存在: %s:%d（同 proxy）", host, port)
		}
	}

	config := adb.ServerConfig{
		Host: host,
		Port: port,
	}
	var proxyCfg *SOCKS5ProxyConfig
	if proxyURL != "" {
		if !strings.HasPrefix(proxyURL, "socks5://") {
			return "", fmt.Errorf("proxy 只支持 socks5:// 格式")
		}
		dialer, err := adb.NewSOCKS5Dialer(proxyURL)
		if err != nil {
			return "", fmt.Errorf("创建 SOCKS5 Dialer 失败: %v", err)
		}
		proxyCfg = &SOCKS5ProxyConfig{URL: proxyURL, Dialer: dialer}
		config.Dialer = dialer
		logutil.Debugf("[EndpointManager] 端点 %s 将通过 SOCKS5 代理连接", id)
	}

	adbClient, err := adb.NewWithConfig(config)
	if err != nil {
		return "", fmt.Errorf("创建 ADB 客户端失败: %v", err)
	}
	if _, err = adbClient.ServerVersion(); err != nil {
		return "", fmt.Errorf("ADB 服务器不可用: %v", err)
	}
	logutil.Debugf("[EndpointManager] adbd 已连接 %s:%d (id=%s)", host, port, id)

	m.endpoints = append(m.endpoints, id)
	m.clients[id] = adbClient
	m.hosts[id] = host
	m.ports[id] = port
	if name != "" {
		m.names[id] = name
	}
	if proxyCfg != nil {
		m.proxies[id] = proxyCfg
	}
	if retry != 0 {
		m.retries[id] = retry
	}
	if !silent {
		logutil.Infof("[EndpointManager] 端点已添加: %s", id)
	}
	return id, nil
}

// RemoveEndpoint 删除端点。入参可为 id（key）或 host:port；按 id 精确删除或按 host:port 匹配第一个。
func (m *Manager) RemoveEndpoint(endpoint string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var foundEndpoint string
	found := false
	if _, exists := m.clients[endpoint]; exists {
		foundEndpoint = endpoint
		found = true
		for i, ep := range m.endpoints {
			if ep == endpoint {
				m.endpoints = append(m.endpoints[:i], m.endpoints[i+1:]...)
				break
			}
		}
	}
	if !found {
		targetHost, targetPort, err := parseEndpoint(endpoint)
		if err != nil {
			return "", fmt.Errorf("解析端点失败: %v", err)
		}
		for i, ep := range m.endpoints {
			if m.hosts[ep] == targetHost && m.ports[ep] == targetPort {
				foundEndpoint = ep
				m.endpoints = append(m.endpoints[:i], m.endpoints[i+1:]...)
				found = true
				break
			}
		}
	}
	if !found {
		return "", fmt.Errorf("端点不存在: %s", endpoint)
	}

	delete(m.clients, foundEndpoint)
	delete(m.hosts, foundEndpoint)
	delete(m.ports, foundEndpoint)
	delete(m.names, foundEndpoint)
	delete(m.proxies, foundEndpoint)
	delete(m.retries, foundEndpoint)
	logutil.Infof("[EndpointManager] 端点已删除: %s", foundEndpoint)
	return foundEndpoint, nil
}

// GetEndpoints 获取所有端点（返回原始格式）
func (m *Manager) GetEndpoints() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]string, len(m.endpoints))
	copy(result, m.endpoints)
	return result
}

// GetEndpointsWithInfo 获取所有端点及其信息（id 为唯一标识，endpoint 为显示名 name 或 id）
func (m *Manager) GetEndpointsWithInfo() []map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]map[string]interface{}, 0, len(m.endpoints))
	for _, id := range m.endpoints {
		host := m.hosts[id]
		port := m.ports[id]
		display := m.names[id]
		if display == "" {
			display = id
		}
		info := map[string]interface{}{
			"id":       id,
			"endpoint": display,
			"host":     host,
			"port":     port,
		}
		if p, ok := m.proxies[id]; ok && p != nil && p.URL != "" {
			info["proxy"] = p.URL
		}
		if r, ok := m.retries[id]; ok && r != 0 {
			info["retry"] = r
		}
		result = append(result, info)
	}
	return result
}

// GetRetry 获取端点的断线重试配置：0 不重连，>0 重试 N 次，<0 一直重试（不填默认 -1）
func (m *Manager) GetRetry(endpoint string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if r, ok := m.retries[endpoint]; ok {
		return r
	}
	return 0
}

// GetEndpointDisplayName 获取端点的显示名称（name 若已设则返回 name，否则返回 id）
func (m *Manager) GetEndpointDisplayName(endpoint string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if n, ok := m.names[endpoint]; ok && n != "" {
		return n
	}
	return endpoint
}

// GetClient 获取端点的 ADB 客户端
func (m *Manager) GetClient(endpoint string) (*adb.Adb, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	client, ok := m.clients[endpoint]
	return client, ok
}

// GetHost 获取端点的 host
func (m *Manager) GetHost(endpoint string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	host, ok := m.hosts[endpoint]
	return host, ok
}

// GetPort 获取端点的 port
func (m *Manager) GetPort(endpoint string) (int, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	port, ok := m.ports[endpoint]
	return port, ok
}

// parseEndpoint 解析端点格式：host:port（不再支持 #别名）。
// 兼容 adb:// 前缀会先去掉再解析。返回 host, port, error
func parseEndpoint(endpoint string) (host string, port int, err error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", 0, fmt.Errorf("端点不能为空")
	}
	endpoint = strings.TrimPrefix(endpoint, "adb://")
	parts := strings.Split(endpoint, ":")
	if len(parts) < 1 || (len(parts) == 1 && strings.TrimSpace(parts[0]) == "") {
		return "", 0, fmt.Errorf("端点格式错误，应为 host 或 host:port")
	}
	host = strings.TrimSpace(parts[0])
	if host == "" {
		return "", 0, fmt.Errorf("host 不能为空")
	}
	port = 5037
	if len(parts) >= 2 {
		portStr := strings.TrimSpace(parts[1])
		if portStr != "" {
			p, e := strconv.Atoi(portStr)
			if e != nil || p <= 0 || p >= 65536 {
				return "", 0, fmt.Errorf("端口无效: %s", portStr)
			}
			port = p
		}
	}
	return host, port, nil
}
