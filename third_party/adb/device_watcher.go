package adb

import (
	"log"
	"math/rand"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ms-robots/ms-robot/internal/logutil"
	"github.com/ms-robots/ms-robot/third_party/adb/internal/errors"
	"github.com/ms-robots/ms-robot/third_party/adb/wire"
)

/*
DeviceWatcher publishes device status change events.
If the server dies while listening for events, it restarts the server.
*/
type DeviceWatcher struct {
	*deviceWatcherImpl
}

// DeviceStateChangedEvent represents a device state transition.
// Contains the device's old and new states, but also provides methods to query the
// type of state transition.
type DeviceStateChangedEvent struct {
	Serial   string
	OldState DeviceState
	NewState DeviceState
}

// CameOnline returns true if this event represents a device coming online.
func (s DeviceStateChangedEvent) CameOnline() bool {
	return s.OldState != StateOnline && s.NewState == StateOnline
}

// WentOffline returns true if this event represents a device going offline.
func (s DeviceStateChangedEvent) WentOffline() bool {
	return s.OldState == StateOnline && s.NewState != StateOnline
}

type deviceWatcherImpl struct {
	server server

	// If an error occurs, it is stored here and eventChan is close immediately after.
	err atomic.Value

	eventChan chan DeviceStateChangedEvent
}

func newDeviceWatcher(server server) *DeviceWatcher {
	watcher := &DeviceWatcher{&deviceWatcherImpl{
		server:    server,
		eventChan: make(chan DeviceStateChangedEvent),
	}}

	runtime.SetFinalizer(watcher, func(watcher *DeviceWatcher) {
		watcher.Shutdown()
	})

	go publishDevices(watcher.deviceWatcherImpl)

	return watcher
}

/*
C returns a channel than can be received on to get events.
If an unrecoverable error occurs, or Shutdown is called, the channel will be closed.
*/
func (w *DeviceWatcher) C() <-chan DeviceStateChangedEvent {
	return w.eventChan
}

// Err returns the error that caused the channel returned by C to be closed, if C is closed.
// If C is not closed, its return value is undefined.
func (w *DeviceWatcher) Err() error {
	if err, ok := w.err.Load().(error); ok {
		return err
	}
	return nil
}

// Shutdown stops the watcher from listening for events and closes the channel returned
// from C.
func (w *DeviceWatcher) Shutdown() {
	// TODO(z): Implement.
}

func (w *deviceWatcherImpl) reportErr(err error) {
	w.err.Store(err)
}

/*
publishDevices reads device lists from scanner, calculates diffs, and publishes events on
eventChan.
Returns when scanner returns an error.
Doesn't refer directly to a *DeviceWatcher so it can be GCed (which will,
in turn, close Scanner and stop this goroutine).

TODO: to support shutdown, spawn a new goroutine each time a server connection is established.
This goroutine should read messages and send them to a message channel. Can write errors directly
to errVal. publishDevicesUntilError should take the msg chan and the scanner and select on the msg chan and stop chan, and if the stop
chan sends, close the scanner and return true. If the msg chan closes, just return false.
publishDevices can look at ret val: if false and err == EOF, reconnect. If false and other error, report err
and abort. If true, report no error and stop.
*/
func publishDevices(watcher *deviceWatcherImpl) {
	defer close(watcher.eventChan)

	var lastKnownStates map[string]DeviceState
	finished := false

	for {
		scanner, err := connectToTrackDevices(watcher.server)
		if err != nil {
			watcher.reportErr(err)
			return
		}

		finished, err = publishDevicesUntilError(scanner, watcher.eventChan, &lastKnownStates)

		if finished {
			scanner.Close()
			return
		}

		if HasErrCode(err, ConnectionResetError) {
			// 连接被重置（如 adb server 重启），等待后重连（本客户端不 exec 启动 server）
			delay := time.Duration(rand.Intn(500)) * time.Millisecond
			log.Printf("[DeviceWatcher] connection reset, reconnecting in %s", delay)
			time.Sleep(delay)
			continue
		} else {
			// Unknown error, don't retry.
			watcher.reportErr(err)
			return
		}
	}
}

func connectToTrackDevices(server server) (wire.Scanner, error) {
	conn, err := server.Dial()
	if err != nil {
		return nil, err
	}

	if err := wire.SendMessageString(conn, "host:track-devices"); err != nil {
		conn.Close()
		return nil, err
	}

	if _, err := conn.ReadStatus("host:track-devices"); err != nil {
		conn.Close()
		return nil, err
	}

	return conn, nil
}

func publishDevicesUntilError(scanner wire.Scanner, eventChan chan<- DeviceStateChangedEvent, lastKnownStates *map[string]DeviceState) (finished bool, err error) {
	for {
		msg, err := scanner.ReadMessage()
		if err != nil {
			return false, err
		}

		msgStr := string(msg)
		if msgStr != "" {
			logutil.Debugf("[DeviceWatcher] track-devices raw payload: %q", msgStr)
		}
		deviceStates, err := parseDeviceStates(msgStr)
		if err != nil {
			return false, err
		}
		if len(deviceStates) > 0 {
			logutil.Debugf("[DeviceWatcher] track-devices parsed %d device(s)", len(deviceStates))
			for serial, st := range deviceStates {
				logutil.Debugf("[DeviceWatcher] track-devices parsed: serial=%q state=%s", serial, st.String())
			}
		}

		for _, event := range calculateStateDiffs(*lastKnownStates, deviceStates) {
			eventChan <- event
		}
		*lastKnownStates = deviceStates
	}
}

func parseDeviceStates(msg string) (states map[string]DeviceState, err error) {
	states = make(map[string]DeviceState)
	lines := strings.Split(msg, "\n")

	for lineNum, line := range lines {
		if len(line) == 0 {
			continue
		}

		fields := strings.Split(line, "\t")
		if len(fields) != 2 {
			logutil.Debugf("[DeviceWatcher] parseDeviceStates invalid line %d (expected 2 tab-separated fields): %q", lineNum, line)
			err = errors.Errorf(errors.ParseError, "invalid device state line %d: %s", lineNum, line)
			return
		}

		deviceSerial := strings.TrimSpace(fields[0])
		stateString := strings.TrimSpace(fields[1])
		logutil.Debugf("[DeviceWatcher] parseDeviceStates line %d: serial=%q state=%q", lineNum, deviceSerial, stateString)

		// adb track-devices 的输出格式是：<设备序列号>\t<状态>
		serial := deviceSerial

		var state DeviceState
		state, err = parseDeviceState(stateString)
		if err != nil {
			logutil.Debugf("[DeviceWatcher] parseDeviceStates line %d unknown state %q, use offline: %v", lineNum, stateString, err)
			state = StateOffline
			err = nil
		}
		states[serial] = state
	}

	return
}

func calculateStateDiffs(oldStates, newStates map[string]DeviceState) (events []DeviceStateChangedEvent) {
	for serial, oldState := range oldStates {
		newState, ok := newStates[serial]

		if oldState != newState {
			if ok {
				// Device present in both lists: state changed.
				events = append(events, DeviceStateChangedEvent{serial, oldState, newState})
			} else {
				// Device only present in old list: device removed.
				events = append(events, DeviceStateChangedEvent{serial, oldState, StateDisconnected})
			}
		}
	}

	for serial, newState := range newStates {
		if _, ok := oldStates[serial]; !ok {
			// Device only present in new list: device added.
			events = append(events, DeviceStateChangedEvent{serial, StateDisconnected, newState})
		}
	}

	return events
}
