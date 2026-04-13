package adb

import (
	"fmt"
	"github.com/ms-robots/ms-robot/third_party/adb/internal/errors"
	"strings"
)

// DeviceState represents one of the 3 possible states adb will report devices.
// A device can be communicated with when it's in StateOnline.
// A USB device will make the following state transitions:
//
//	Plugged in: StateDisconnected->StateOffline->StateOnline
//	Unplugged:  StateOnline->StateDisconnected
//
//go:generate stringer -type=DeviceState
type DeviceState int8

const (
	StateInvalid DeviceState = iota
	StateUnauthorized
	StateDisconnected
	StateOffline
	StateConnecting
	StateOnline
)

var deviceStateStrings = map[string]DeviceState{
	"":             StateDisconnected,
	"offline":      StateOffline,
	"connecting":   StateConnecting,
	"authorizing":  StateConnecting, // authorizing 是 connecting 的一种状态
	"device":       StateOnline,
	"unauthorized": StateUnauthorized,
}

func parseDeviceState(str string) (DeviceState, error) {
	str = strings.TrimSpace(str)

	state, ok := deviceStateStrings[str]
	if !ok {
		// 对于未知状态，返回 StateOffline 而不是 StateInvalid，更健壮
		// 注意：这里返回错误，但调用方会处理并继续
		errMsg := fmt.Sprintf("invalid device state: %q (length: %d)", str, len(str))
		return StateOffline, errors.Errorf(errors.ParseError, errMsg)
	}
	return state, nil
}
