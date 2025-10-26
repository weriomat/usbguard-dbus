package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/gorilla/mux"

	"github.com/godbus/dbus/v5"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"

	"golang.org/x/net/context/ctxhttp"
	"golang.org/x/sync/errgroup"
)

var (
	re         = regexp.MustCompile(`name "\S[^"]+"`)
	ErrNoItems = errors.New("No usb device available")
)

func listen_dbus(ctx context.Context, logger log.Logger, mm *manager) error {
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

			// TODO: use a state map and track if a item was removed -> remove from queue
			// check if device was inserted (inserted == 1, removed == 3), see: https://github.com/USBGuard/usbguard/blob/main/src/DBus/DBusInterface.xml#L169
			if state != 1 {
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

			mm.addEntry(id)
			level.Info(logger).Log("msg", "Added entry", "name", name, "id", id)
		case <-ctx.Done():
			level.Info(logger).Log("msg", "parent ctx finished")
			return nil

		}
	}
}

// Dbus generated msg
type dWrite struct {
	id int
}

// http api generated msg
type dRead struct {
	resp chan int
}

// Replace with a direct canel and a global counter -> for early reject?
type manager struct {
	wC       int64
	state    chan int
	dbus     chan dWrite
	requests chan dRead
}

func newManager() *manager {
	return &manager{
		// Queue
		state: make(chan int, 100),
		// add item to queue
		dbus: make(chan dWrite),
		// request item
		requests: make(chan dRead),
		wC:       0,
	}
}

func (m *manager) start(ctx context.Context, logger log.Logger) error {
	for {
		select {
		case write := <-m.dbus:
			m.state <- write.id
		case read := <-m.requests:
			item := <-m.state
			read.resp <- item
		case <-ctx.Done():
			level.Info(logger).Log("msg", "finished manager")
			return nil
		}
	}
}

func (m *manager) addEntry(id int) {
	a := dWrite{
		id: id,
	}

	m.dbus <- a

	atomic.AddInt64(&m.wC, 1)
}

func (m *manager) requestEntry() (int, error) {
	if atomic.LoadInt64(&m.wC) <= 0 {
		return 0, ErrNoItems
	}

	cc := dRead{
		resp: make(chan int),
	}

	m.requests <- cc

	id := <-cc.resp

	atomic.AddInt64(&m.wC, -1)
	return id, nil
}

// arg should be either 0: accept/ 2: reject
func (m *manager) makeDbusCall(arg int) (string, error) {
	id, err := m.requestEntry()

	if err != nil {
		return "", err
	}

	ids := fmt.Sprintf("{ID: %d}", id)

	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return "", err
	}
	defer conn.Close()

	// arguments: id of device, 0 == accept, 2 == reject, non permanent: https://github.com/USBGuard/usbguard/blob/main/src/DBus/DBusInterface.xml#L141
	conn.Object("org.usbguard1", "/org/usbguard1/Devices").Call("org.usbguard.Devices1.applyDevicePolicy", 0, uint32(id), uint32(arg), false)
	return ids, nil
}

func (m *manager) reject() (string, error) {
	id, err := m.makeDbusCall(2)

	if err != nil {
		return "", err
	}

	return id, nil
}

func (m *manager) accept() (string, error) {
	id, err := m.makeDbusCall(0)

	if err != nil {
		return "", err
	}

	return id, nil
}

func webserver(ctx context.Context, logger log.Logger, socketPath string, mm *manager) error {
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	socket, err := net.Listen("unix", socketPath)

	if err != nil {
		level.Error(logger).Log("msg", "Failed to listen on unix socket")
		return err
	}

	defer socket.Close()

	m := mux.NewRouter()

	m.HandleFunc("/accept", func(w http.ResponseWriter, r *http.Request) {
		msg, err := mm.accept()

		if err != nil {
			w.Write([]byte(err.Error()))
		} else {
			w.Write([]byte(msg))
		}
	}).Methods(http.MethodGet)

	m.HandleFunc("/reject", func(w http.ResponseWriter, r *http.Request) {
		msg, err := mm.reject()

		if err != nil {
			w.Write([]byte(err.Error()))
		} else {
			w.Write([]byte(msg))
		}
	}).Methods(http.MethodGet)

	srv := http.Server{Handler: m}

	eg, ctx := errgroup.WithContext(subCtx)

	// clean shutdown
	eg.Go(func() error {
		defer cancel()

		<-ctx.Done()

		innerEg, ctx := errgroup.WithContext(ctx)

		innerEg.Go(func() error {
			err := srv.Shutdown(ctx)
			if err != nil {
				level.Error(logger).Log("msg", "Failed to stop webserver", "err", err)
			} else {
				level.Info(logger).Log("msg", "Stopped webserver")
			}
			return err
		})

		return innerEg.Wait()
	})

	// start webserver
	eg.Go(func() error {
		defer cancel()

		level.Info(logger).Log("msg", "Starting webserver", "address", socket)

		err := srv.Serve(socket)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			level.Error(logger).Log("msg", "Failed to listen in webserver", "err", err)
		} else {
			level.Info(logger).Log("msg", "Webserver finished listening")
		}

		return err
	})

	return eg.Wait()
}

func dialApi(ctx context.Context, logger log.Logger, socketPath string, endpoint string) error {
	httpc := http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
	}

	req, err := ctxhttp.Get(ctx, &httpc, "http://unix"+endpoint)
	if err != nil {
		level.Error(logger).Log("msg", "Failed to make accept request", "err", err)
	} else {
		defer req.Body.Close()
		body, err := io.ReadAll(req.Body)
		if err != nil {
			level.Error(logger).Log("msg", "Failed to read body of request", "err", err)
			return err
		}
		level.Info(logger).Log("msg", string(body))
	}

	return err
}

func main() {
	accept := flag.Bool("accept", false, "send the accept command to the api")
	reject := flag.Bool("reject", false, "send the reject command to the api")
	socketPath := flag.String("socket", "./usbguard-dbus.sock", "the path of the socket to use")
	flag.Parse()

	if *accept && *reject {
		fmt.Println("Cannot accept and reject at the same time")
		os.Exit(1)
	}

	w := log.NewSyncWriter(os.Stderr)
	logger := log.With(log.NewLogfmtLogger(w), "ts", log.DefaultTimestampUTC)

	ctx, cancel := context.WithCancel(context.Background())
	eg, ctx := errgroup.WithContext(ctx)

	if *accept {
		eg.Go(func() error {
			defer cancel()

			logger := log.With(logger, "component", "accept")

			err := dialApi(ctx, logger, *socketPath, "/accept")

			return err
		})
	} else if *reject {
		eg.Go(func() error {
			defer cancel()

			logger := log.With(logger, "component", "accept")

			err := dialApi(ctx, logger, *socketPath, "/reject")

			return err
		})
	} else {
		manager := newManager()

		eg.Go(func() error {
			defer cancel()

			logger := log.With(logger, "component", "dbus-listener")

			level.Info(logger).Log("msg", "Dbus listener started")

			err := listen_dbus(ctx, logger, manager)

			if err == nil {
				level.Info(logger).Log("msg", "Stopped dbus listener")
			} else {
				level.Error(logger).Log("msg", "Stopped dbus listener", "err", err)
			}

			return err

		})

		eg.Go(func() error {
			defer cancel()

			logger := log.With(logger, "component", "webserver")

			level.Info(logger).Log("msg", "Webserver started")

			err := webserver(ctx, logger, *socketPath, manager)

			if err == nil {
				level.Info(logger).Log("msg", "Stopped webserver")
			} else {
				level.Error(logger).Log("msg", "Stopped webserver", "err", err)
			}

			return err

		})

		eg.Go(func() error {
			defer cancel()

			logger := log.With(logger, "component", "manager")

			level.Info(logger).Log("msg", "Manager started")

			err := manager.start(ctx, logger)

			if err == nil {
				level.Info(logger).Log("msg", "Stopped manager")
			} else {
				level.Error(logger).Log("msg", "Stopped manager", "err", err)
			}

			return err
		})
	}

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
