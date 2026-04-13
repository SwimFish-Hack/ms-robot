package device

import (
	"fmt"

	adb "github.com/ms-robots/ms-robot/third_party/adb"
)

// AdbClient 返回唯一的 third_party adb 客户端
func (m *Manager) AdbClient() *adb.Adb {
	return m.adbServer
}

// GetADBDevice 根据 key（serial 或 serial:transportID）获取 *adb.Device
func (m *Manager) GetADBDevice(key string) (*adb.Device, error) {
	if m.adbServer == nil {
		return nil, fmt.Errorf("ADB 不可用")
	}
	serial, transportID := parseDeviceKey(key)
	if _, ok := m.GetDevice(key); !ok {
		return nil, fmt.Errorf("设备未找到: %s", key)
	}
	return m.adbDeviceForSerialTransport(serial, transportID), nil
}
