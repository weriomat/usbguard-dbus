package main

// Modeled after: https://websocket.org/guides/languages/go/#client-implementation
import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gorilla/websocket"
)

type webSocketClient struct {
	logger         log.Logger
	URL            url.URL
	socketPath     string
	conn           *websocket.Conn
	receive        chan []byte
	reconnect      bool
	reconnectDelay time.Duration
	maxReconnect   time.Duration
	mu             sync.Mutex
}

func newWebSocketClient(logger log.Logger, serverURL url.URL, socketPath string) *webSocketClient {
	return &webSocketClient{
		URL:            serverURL,
		receive:        make(chan []byte, 100),
		reconnect:      true,
		reconnectDelay: 2 * time.Second,
		maxReconnect:   30 * time.Second,
		socketPath:     socketPath,
		conn:           &websocket.Conn{},
		mu:             sync.Mutex{},
		logger:         logger,
	}
}

func (c *webSocketClient) Connect() error {
	dialer := websocket.Dialer{
		NetDial:          func(network, addr string) (net.Conn, error) { return net.Dial("unix", c.socketPath) },
		HandshakeTimeout: 5 * time.Second,
	}

	conn, _, err := dialer.Dial(c.URL.String(), nil)
	if err != nil {
		level.Error(c.logger).Log("msg", "Failed to dial unix domain socket", "err", err)
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	// Start read and write pumps
	go c.readPump()

	return nil
}

func (c *webSocketClient) readPump() {
	defer func() {
		c.mu.Lock()
		if c.conn != nil {
			c.conn.Close()
			c.conn = nil
		}
		c.mu.Unlock()

		if c.reconnect {
			go c.reconnectLoop()
		}
	}()

	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway,
				websocket.CloseAbnormalClosure) {
				level.Error(c.logger).Log("msg", "Websocketed failed", "err", err)
			}
			return
		}

		c.receive <- message
	}
}

func (c *webSocketClient) reconnectLoop() {
	delay := c.reconnectDelay

	for c.reconnect {
		time.Sleep(delay)

		if err := c.Connect(); err != nil {
			level.Error(c.logger).Log("msg", "failed to reconnect", "err", err)

			// Exponential backoff
			delay *= 2
			if delay > c.maxReconnect {
				delay = c.maxReconnect
			}
		} else {
			return
		}
	}

}

func (c *webSocketClient) Receive() ([]byte, error) {
	select {
	case message := <-c.receive:
		return message, nil
	case <-time.After(2 * time.Second):
		return nil, fmt.Errorf("receive timeout")
	}
}

func (c *webSocketClient) Close() {
	c.reconnect = false

	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.mu.Unlock()
}

func startListenerClient(ctx context.Context, logger log.Logger, socketPath string) error {
	u := url.URL{Scheme: "ws", Host: "unix", Path: "/ws"}

	client := newWebSocketClient(logger, u, socketPath)

	if err := client.Connect(); err != nil {
		level.Error(logger).Log("msg", "Failed to connect to client", "err", err)
		return err
	}
	defer client.Close()

	fmt.Println("{\"text\": \"\"}")

	for {
		select {
		case <-ctx.Done():
			level.Info(logger).Log("msg", "Client partent ctx finished")
			client.reconnect = false
			client.Close()
			return nil
		default:
			message, err := client.Receive()
			if err != nil {
				continue
			}

			fmt.Println(string(message))
		}
	}
}
