package main

// Modeled after: https://github.com/gorilla/websocket/tree/main/examples/chat and https://websocket.org/guides/languages/go/#concurrent-connection-management
import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"sync"
	"time"

	"github.com/coreos/go-systemd/activation"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"golang.org/x/sync/errgroup"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 512
)

var (
	upgrader = websocket.Upgrader{
		ReadBufferSize: 1024, WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool { // Configure origin checking for production
			return true
		},
	}
)

func generateID() string {
	return fmt.Sprintf("client_%d", time.Now().UnixNano())
}

type client struct {
	ID  string
	hub *hub

	// The websocket connection.
	conn *websocket.Conn

	// Buffered channel of outbound messages.
	send chan []byte
}

func (c *client) readPump() {
	defer func() {
		level.Info(c.hub.logger).Log("msg", "Closing read pump of client", "client", c.ID)
		c.hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { c.conn.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				level.Error(c.hub.logger).Log("msg", "WS error", "err", err)
			}
			break
		}

		level.Info(c.hub.logger).Log("msg", "got msg from client", "message", message, "client", c.ID)
	}
}

func (c *client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			c.conn.WriteMessage(websocket.TextMessage, message)
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// serveWs handles websocket requests from the peer.
func (h *hub) serveWs(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		level.Error(h.logger).Log("msg", "Failed to upgrade connection", "err", err)
		return
	}

	// Get client ID from query params or generate
	clientID := r.URL.Query().Get("id")
	if clientID == "" {
		clientID = generateID()
	}

	client := &client{
		hub:  h,
		conn: conn,
		send: make(chan []byte, 256),
		ID:   clientID,
	}

	h.register <- client

	// Allow collection of memory referenced by the caller by doing all work in
	// new goroutines.
	go client.writePump()
	go client.readPump()
}

type hub struct {
	// Context
	ctx context.Context

	// Logger
	logger log.Logger
	// Registered clients.
	clients map[*client]bool

	// Inbound messages from the clients.
	broadcast chan []byte

	// Register requests from the clients.
	register chan *client

	// Unregister requests from clients.
	unregister chan *client
	mu         sync.RWMutex
}

func newHub(ctx context.Context, logger log.Logger) *hub {
	return &hub{
		broadcast:  make(chan []byte),
		register:   make(chan *client),
		unregister: make(chan *client),
		clients:    make(map[*client]bool),
		ctx:        ctx,
		logger:     logger,
		mu:         sync.RWMutex{},
	}
}

func (h *hub) run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			level.Info(h.logger).Log("msg", "Client registered", "id", client.ID)
		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			h.mu.Unlock()
			level.Info(h.logger).Log("msg", "Client unregistered", "id", client.ID)
		case message := <-h.broadcast:
			h.broadcastMessage(message, nil)
		case <-h.ctx.Done():
			level.Info(h.logger).Log("msg", "Hub context done")
			return
		}
	}
}

func (h *hub) broadcastMessage(message []byte, exclude *client) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	level.Info(h.logger).Log("msg", "broadcasting message", "message", message)
	for client := range h.clients {
		if client != exclude {
			select {
			case client.send <- message:
			default:
				// Client's send channel is full, close it
				delete(h.clients, client)
				close(client.send)
			}
		}
	}

}

func webserver(ctx context.Context, logger log.Logger, mm *manager) error {
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

	hub := newHub(ctx, logger)
	mm.addHub(hub)

	m.HandleFunc("/ws", hub.serveWs)

	srv := http.Server{Handler: m}

	eg, ctx := errgroup.WithContext(subCtx)

	eg.Go(func() error {
		defer cancel()

		hub.run()
		return nil
	})

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

		level.Info(logger).Log("msg", "Starting webserver", "address", socket.Addr().String())

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
