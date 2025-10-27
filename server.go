package main

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gorilla/mux"
	"golang.org/x/sync/errgroup"
)

func webserver(ctx context.Context, logger log.Logger, socketPath string, mm *Manager) error {
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sD := os.Getenv("XDG_RUNTIME_DIR")

	// check if dir exists
	_, err := os.Stat(sD)
	if err != nil {
		level.Error(logger).Log("msg", "XDG_RUNTIME_DIR does not exist", "err", err)
		return err
	}

	listeners, err := activation.Listeners()
	if err != nil {
		level.Error(logger).Log("msg", "Failed to receive activation listener from systemd, creating own", "err", err)
	}

	var socket net.Listener
	if len(listeners) >= 1 {
		socket = listeners[0]
	} else {
		socketPath := path.Join(sD, "usbguard-dbus.sock")

		socket, err = net.Listen("unix", socketPath)
		if err != nil {
			level.Error(logger).Log("msg", "Failed to listen on unix socket")
			return err
		}
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
