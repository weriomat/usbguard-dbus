package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"

	"golang.org/x/sync/errgroup"
)

var (
	re = regexp.MustCompile(`name "\S[^"]+"`)
)

func listen_dbus(ctx context.Context, logger log.Logger) error {
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
		case v := <-c:

			// We are only interested if a new device appears
			if v.Name != "org.usbguard.Devices1.DevicePresenceChanged" {
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

			// check if device was inserted (inserted == 1, removed == 3), see: https://github.com/USBGuard/usbguard/blob/main/src/DBus/DBusInterface.xml#L169
			if state != 1 {
				continue
			}

			// remove `name "*"`
			name := strings.TrimRight(strings.TrimPrefix(re.FindString(fmt.Sprint(v.Body)), `name "`), `"`)
			fmt.Println(name)

			// get the ID
			id, err := strconv.Atoi(fmt.Sprint(v.Body[0]))

			if err != nil {
				level.Error(logger).Log(
					"msg", "Failed to convert the id of the device to a int",
					"err", err,
				)
				continue
			}

			fmt.Println("ID: ", id)

			var s uint32
			obj := conn.Object("org.usbguard1", "/org/usbguard1/Devices")
			// arguments: id of device, 0 == allow, non permanent: https://github.com/USBGuard/usbguard/blob/main/src/DBus/DBusInterface.xml#L141
			err = obj.CallWithContext(ctx, "org.usbguard.Devices1.applyDevicePolicy", 0, uint32(id), uint32(0), false).Store(&s)

			if err != nil {
				level.Error(logger).Log(
					"msg", "Failed to allow the device",
					"err", err,
				)

				continue
			}
		case <-ctx.Done():
			level.Info(logger).Log("msg", "parent ctx finished")
			return nil

		}
	}
}

func main() {
	w := log.NewSyncWriter(os.Stderr)
	logger := log.With(log.NewLogfmtLogger(w), "ts", log.DefaultTimestampUTC)

	ctx, cancel := context.WithCancel(context.Background())
	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		defer cancel()

		logger := log.With(logger, "component", "dbus-listener")

		err := listen_dbus(ctx, logger)

		if err == nil {
			level.Info(logger).Log("msg", "Stopped dbus listener")
		} else {
			level.Error(logger).Log("msg", "Stopped dbus listener", "err", err)
		}

		return err

	})

	eg.Go(func() error {
		logger := log.With(logger, "component", "signal")

		// setup listener channel
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		defer signal.Ignore(os.Interrupt)
		defer cancel()

		select {
		case sig := <-sig:
			level.Info(logger).Log("msg", "received signal", "sig", sig)
			return nil
		case <-ctx.Done():
			level.Info(logger).Log("msg", "parent ctx finished")
			return nil
		}
	})

	err := eg.Wait()
	if err == nil {
		level.Info(logger).Log("msg", "dbus listener shutdown gracefully")
	} else {
		level.Error(logger).Log("msg", "dbus listener did not shutdown gracefully", "err", err)

		os.Exit(1)
	}
}
