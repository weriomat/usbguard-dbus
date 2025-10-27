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

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"

	"golang.org/x/net/context/ctxhttp"
	"golang.org/x/sync/errgroup"
)

var (
	re         = regexp.MustCompile(`name "\S[^"]+"`)
	ErrNoItems = errors.New("No usb device available")
)

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
	listen := flag.Bool("listen", false, "listen to incoming messages using a websocket over a unix socket")
	socketPath := flag.String("socket", "./usbguard-dbus.sock", "the path of the socket to use (only for accept/ reject)")
	flag.Parse()

	if (*accept && *reject) || (*accept && *listen) || (*reject && *listen) {
		fmt.Println("Cannot accept and reject at the same time")
		os.Exit(1)
	}

	w := log.NewSyncWriter(os.Stderr)
	logger := log.With(log.NewLogfmtLogger(w), "ts", log.DefaultTimestampUTC)

	ctx, cancel := context.WithCancel(context.Background())
	eg, ctx := errgroup.WithContext(ctx)

	if *listen {
		eg.Go(func() error {
			defer cancel()

			logger := log.With(logger, "component", "client")

			err := startListenerClient(ctx, logger, *socketPath)

			return err
		})
	} else if *accept {
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

			err := webserver(ctx, logger, manager)

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
