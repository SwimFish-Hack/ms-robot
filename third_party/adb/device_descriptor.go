package adb

import "fmt"

// 与 adb 命令行、adbd 协议的对应关系：
//
//	adb -s SERIAL   →  host-serial:SERIAL:<request>  或  host:transport:SERIAL 再发 <request>
//	adb -t ID       →  host-transport-id:ID:<request> 或  host:transport-id:ID 再发 <request>
//
// 即：-s 用 serial 选设备，协议里用 host-serial 或 transport:SERIAL；
// -t 用 transport id（adb devices -l 的 transport_id）选设备，协议里用 host-transport-id 或 transport-id:ID。
// 不能把 "serial:transportID" 整串当 serial 传给协议，否则会变成 host-serial:xxx:1，adbd 无法识别。
//
//go:generate stringer -type=deviceDescriptorType
type deviceDescriptorType int

const (
	// host:transport-any and host:<request>
	DeviceAny deviceDescriptorType = iota
	// -s SERIAL → host-serial:<serial>:<request>，serial 仅为设备序列号（不含 :transportID）
	DeviceSerial
	// host:transport-usb and host-usb:<request>
	DeviceUsb
	// host:transport-local and host-local:<request>
	DeviceLocal
	// -t ID → host-transport-id:<id>:<request>（ID 来自 adb devices -l 的 transport_id）
	DeviceTransportID
)

type DeviceDescriptor struct {
	descriptorType deviceDescriptorType

	// Only used if Type is DeviceSerial.
	serial string

	// Only used if Type is DeviceTransportID.
	transportID int
}

func AnyDevice() DeviceDescriptor {
	return DeviceDescriptor{descriptorType: DeviceAny}
}

func AnyUsbDevice() DeviceDescriptor {
	return DeviceDescriptor{descriptorType: DeviceUsb}
}

func AnyLocalDevice() DeviceDescriptor {
	return DeviceDescriptor{descriptorType: DeviceLocal}
}

func DeviceWithSerial(serial string) DeviceDescriptor {
	return DeviceDescriptor{
		descriptorType: DeviceSerial,
		serial:         serial,
	}
}

// DeviceWithTransportID selects device by transport ID (from adb devices -l). Use when serial is ambiguous.
func DeviceWithTransportID(transportID int) DeviceDescriptor {
	return DeviceDescriptor{
		descriptorType: DeviceTransportID,
		transportID:    transportID,
	}
}

func (d DeviceDescriptor) String() string {
	switch d.descriptorType {
	case DeviceSerial:
		return fmt.Sprintf("%s[%s]", d.descriptorType, d.serial)
	case DeviceTransportID:
		return fmt.Sprintf("%s[%d]", d.descriptorType, d.transportID)
	}
	return d.descriptorType.String()
}

func (d DeviceDescriptor) getHostPrefix() string {
	switch d.descriptorType {
	case DeviceAny:
		return "host"
	case DeviceUsb:
		return "host-usb"
	case DeviceLocal:
		return "host-local"
	case DeviceSerial:
		return fmt.Sprintf("host-serial:%s", d.serial)
	case DeviceTransportID:
		return fmt.Sprintf("host-transport-id:%d", d.transportID)
	default:
		panic(fmt.Sprintf("invalid DeviceDescriptorType: %v", d.descriptorType))
	}
}

func (d DeviceDescriptor) getTransportDescriptor() string {
	switch d.descriptorType {
	case DeviceAny:
		return "transport-any"
	case DeviceUsb:
		return "transport-usb"
	case DeviceLocal:
		return "transport-local"
	case DeviceSerial:
		return fmt.Sprintf("transport:%s", d.serial)
	case DeviceTransportID:
		return fmt.Sprintf("transport-id:%d", d.transportID)
	default:
		panic(fmt.Sprintf("invalid DeviceDescriptorType: %v", d.descriptorType))
	}
}
