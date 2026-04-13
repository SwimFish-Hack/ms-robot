package adb

import (
	"bufio"
	"strconv"
	"strings"

	"github.com/ms-robots/ms-robot/internal/logutil"
	"github.com/ms-robots/ms-robot/third_party/adb/internal/errors"
)

func parseInt(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err
}

type DeviceInfo struct {
	// Always set.
	Serial string
	Status string

	// Product, device, and model are not set in the short form.
	Product    string
	Model      string
	DeviceInfo string

	// Only set for devices connected via USB.
	Usb string

	// TransportID from adb devices -l (transport_id:N). 0 when not set.
	TransportID int
}

// IsUsb returns true if the device is connected via USB.
func (d *DeviceInfo) IsUsb() bool {
	return d.Usb != ""
}

func newDevice(serial string, status string, attrs map[string]string) (*DeviceInfo, error) {
	if serial == "" {
		return nil, errors.AssertionErrorf("device serial cannot be blank")
	}
	transportID := 0
	if s := attrs["transport_id"]; s != "" {
		if n, err := parseInt(s); err == nil && n >= 0 {
			transportID = n
		}
	}
	return &DeviceInfo{
		Serial:      serial,
		Status:      status,
		Product:     attrs["product"],
		Model:       attrs["model"],
		DeviceInfo:  attrs["device"],
		Usb:         attrs["usb"],
		TransportID: transportID,
	}, nil
}

func parseDeviceList(list string, lineParseFunc func(string) (*DeviceInfo, error)) ([]*DeviceInfo, error) {
	logutil.Debugf("[adb] devices list raw payload: %q", list)
	devices := []*DeviceInfo{}
	scanner := bufio.NewScanner(strings.NewReader(list))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			// 兼容尾部空行/中间空行
			continue
		}
		device, err := lineParseFunc(line)
		if err != nil {
			// 兼容异常单行：跳过并继续解析其他设备，避免整批失败。
			logutil.Debugf("[adb] parseDeviceList skip malformed line: %q -> %v", line, err)
			continue
		}
		logutil.Debugf("[adb] parseDeviceList parsed: serial=%q status=%q transport_id=%d", device.Serial, device.Status, device.TransportID)
		devices = append(devices, device)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return devices, nil
}

func parseDeviceShort(line string) (*DeviceInfo, error) {
	fields := strings.Fields(line)
	if len(fields) != 2 {
		logutil.Debugf("[adb] parseDeviceShort malformed line (expected 2 fields, got %d): %q", len(fields), line)
		return nil, errors.Errorf(errors.ParseError,
			"malformed device line, expected 2 fields but found %d", len(fields))
	}

	return newDevice(fields[0], fields[1], map[string]string{})
}

func parseDeviceLong(line string) (*DeviceInfo, error) {
	fields := strings.Fields(line)
	// 兼容 adb devices -l 的短行输出（如: "<serial> offline"）
	// serial + status 是最小必需字段，其余属性（product/model/device/transport_id...）可选。
	if len(fields) < 2 {
		logutil.Debugf("[adb] parseDeviceLong malformed line (expected at least 2 fields, got %d): %q", len(fields), line)
		return nil, errors.Errorf(errors.ParseError,
			"malformed device line, expected at least 2 fields but found %d", len(fields))
	}

	attrs := map[string]string{}
	if len(fields) > 2 {
		attrs = parseDeviceAttributes(fields[2:])
	}
	return newDevice(fields[0], fields[1], attrs)
}

func parseDeviceAttributes(fields []string) map[string]string {
	attrs := map[string]string{}
	for _, field := range fields {
		key, val, ok := parseKeyVal(field)
		if !ok {
			// 兼容异常 token（无 key:value 结构），跳过而不是中断解析。
			continue
		}
		attrs[key] = val
	}
	return attrs
}

// Parses a key:val pair and returns key, val.
func parseKeyVal(pair string) (string, string, bool) {
	split := strings.SplitN(pair, ":", 2)
	if len(split) != 2 || split[0] == "" {
		return "", "", false
	}
	return split[0], split[1], true
}
