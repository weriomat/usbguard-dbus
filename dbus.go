package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/godbus/dbus/v5"
)

func listenDbus(ctx context.Context, logger log.Logger, mm *manager) error {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		level.Error(logger).Log(
			"msg", "Failed to connect to system bus",
			"err", err,
		)

		return err
	}
	defer conn.Close()

	if err = conn.AddMatchSignalContext(ctx,
		dbus.WithMatchObjectPath("/org/usbguard1/Devices"),
		dbus.WithMatchInterface("org.usbguard.Devices1"),
	); err != nil {
		level.Error(logger).Log(
			"msg", "Failed to add signal handler",
			"err", err,
		)

		return err
	}

	c := make(chan *dbus.Signal, 10)
	conn.Signal(c)
	level.Info(logger).Log("msg", "Started to listen to attaching devices")

	for {
		select {
		// Add if device policy of block (default) was applied, remove if device is removed/ allowed or rejected
		case v := <-c:
			// We are not interested if a policy changed
			if v.Name == "org.usbguard.Devices1.DevicePolicyChanged" {
				continue
			}

			state, err := strconv.Atoi(fmt.Sprint(v.Body[1]))

			if err != nil {
				level.Error(logger).Log(
					"msg", "Failed to convert the state to a int",
					"err", err,
				)
				continue
			}

			// remove `name "*"`
			name := strings.TrimRight(strings.TrimPrefix(re.FindString(fmt.Sprint(v.Body)), `name "`), `"`)

			// get the ID
			id, err := strconv.Atoi(fmt.Sprint(v.Body[0]))

			if err != nil {
				level.Error(logger).Log(
					"msg", "Failed to convert the id of the device to a int",
					"err", err,
				)
				continue
			}

			// Presence: check if device was inserted (inserted == 1, removed == 3), see: https://github.com/USBGuard/usbguard/blob/main/src/DBus/DBusInterface.xml#L169
			if v.Name == "org.usbguard.Devices1.DevicePresenceChanged" && state == 3 {
				mm.removeEntry(id, name)
				level.Info(logger).Log("msg", "Removed entry due to Device being removed", "name", name, "id", id)
				continue
			}

			switch state {
			case 0: // Allow
				mm.removeEntry(id, name)
				level.Info(logger).Log("msg", "Removed entry due to PolicyApplied", "name", name, "id", id, "state", state)
			case 1: // Block
				mm.addEntry(id, name)
				level.Info(logger).Log("msg", "Added entry due to PolicyApplied", "name", name, "id", id, "state", state)
			case 2: // Reject
				mm.removeEntry(id, name)
				level.Info(logger).Log("msg", "Removed entry due to PolicyApplied", "name", name, "id", id, "state", state)
			}
		case <-ctx.Done():
			level.Info(logger).Log("msg", "parent ctx finished")
			return nil
		}
	}
}
