// Package tunnel carries multiplexed HTTP between a chip and the chipin server.
//
// The chip always dials out (so it works behind NAT with no open ports) over a
// WebSocket, and yamux runs on top of it. The server opens streams; the chip
// accepts them and serves plain HTTP/1.1 on each one.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

const (
	Path          = "/tunnel"
	VersionHeader = "X-Chipin-Chip-Version"
)

// ErrUnauthorized means the server rejected the chip token; retrying won't help.
var ErrUnauthorized = errors.New("chip token rejected by server")

func yamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = true
	c.KeepAliveInterval = 15 * time.Second
	c.ConnectionWriteTimeout = 30 * time.Second
	c.MaxStreamWindowSize = 1 << 20
	c.LogOutput = io.Discard
	return c
}

// Dial connects a chip to the server. The returned session is a net.Listener
// that yields one connection per request stream opened by the server.
func Dial(ctx context.Context, serverURL, token, version string) (*yamux.Session, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("bad server url: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + Path

	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set(VersionHeader, version)
	ws, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return nil, ErrUnauthorized
		}
		return nil, err
	}
	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	sess, err := yamux.Server(conn, yamuxConfig())
	if err != nil {
		conn.Close()
		return nil, err
	}
	return sess, nil
}

// Accept upgrades a chip's request into a session the server opens streams on.
func Accept(w http.ResponseWriter, r *http.Request) (*yamux.Session, error) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, err
	}
	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	sess, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		conn.Close()
		return nil, err
	}
	return sess, nil
}
