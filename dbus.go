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

func listen_dbus(ctx context.Context, logger log.Logger, mm *Manager) error {
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
		// Add/ remove device if Presence changed + remove device if Policy to allow/ reject was applied
		case v := <-c:
			// We are only interested if a new device appears/ Policy to allow/reject was applied
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

			// Presence: check if device was inserted (inserted == 1, removed == 3), see: https://github.com/USBGuard/usbguard/blob/main/src/DBus/DBusInterface.xml#L169
			if v.Name == "org.usbguard.Devices1.DevicePresenceChanged" && state != 1 && state != 3 {
				continue
			}

			// PolicyApplied: check if device is allowed (0)/ rejected (block == 1), see: https://github.com/USBGuard/usbguard/blob/main/src/DBus/DBusInterface.xml#L243
			if v.Name == "org.usbguard.Devices1.DevicePolicyApplied" && state == 1 {
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

			// Device was allowed/ rejected (possible manually via usbguard cli)
			if v.Name == "org.usbguard.Devices1.DevicePolicyApplied" {
				mm.removeEntry(id, name)
				level.Info(logger).Log("msg", "Removed entry due to PolicyApplied", "name", name, "id", id, "state", state)
				continue
			}

			switch state {
			case 1:
				mm.addEntry(id)
				fmt.Printf("{\"text\": %s}\n", name)
				level.Info(logger).Log("msg", "Added entry", "name", name, "id", id)
			case 3:
				mm.removeEntry(id)
				fmt.Printf("{\"text\": \"\"}\n")
				level.Info(logger).Log("msg", "Removed entry", "name", name, "id", id)
			}

		case <-ctx.Done():
			level.Info(logger).Log("msg", "parent ctx finished")
			return nil
		}
	}
}
