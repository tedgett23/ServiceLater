package realtime

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/auth"
)

const (
	maxConnsPerUser = 10
	writeTimeout    = 5 * time.Second
	pingEvery       = 25 * time.Second

	// Application close codes (4000-4999 are ours to define).
	CloseSessionExpired = websocket.StatusCode(4401)
	CloseTooSlow        = websocket.StatusCode(4408)
)

type client struct {
	userID uuid.UUID
	staff  bool
	send   chan []byte

	kicked   chan struct{}
	kickOnce sync.Once
	down     chan struct{}
	downOnce sync.Once
}

func (c *client) kick()     { c.kickOnce.Do(func() { close(c.kicked) }) }
func (c *client) shutdown() { c.downOnce.Do(func() { close(c.down) }) }

func (h *Hub) add(c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.perUser[c.userID] >= maxConnsPerUser {
		return false
	}
	h.perUser[c.userID]++
	h.clients[c] = struct{}{}
	return true
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	if h.perUser[c.userID]--; h.perUser[c.userID] <= 0 {
		delete(h.perUser, c.userID)
	}
}

// Serve upgrades an authenticated request to a WebSocket and streams events to it until the connection
// drops, the hub shuts down, the client falls behind, or the access token expires. The token expiry closes
// the socket so a changed role or deleted user cannot keep receiving events beyond the token's lifetime.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	c := &client{
		userID: p.UserID, staff: p.Role == "admin" || p.Role == "agent",
		send: make(chan []byte, sendBuffer), kicked: make(chan struct{}), down: make(chan struct{}),
	}
	if !h.add(c) {
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}
	defer h.remove(c)

	// Accept rejects cross-origin browsers: Origin must match Host, like the REST CSRF check.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()

	ctx := conn.CloseRead(r.Context()) // we only send; this services pings, pongs and close frames
	expiry := time.NewTimer(time.Until(p.Expires))
	defer expiry.Stop()
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()

	write := func(msg []byte) error {
		wctx, cancel := context.WithTimeout(ctx, writeTimeout)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, msg)
	}

	for {
		select {
		case msg := <-c.send:
			if write(msg) != nil {
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		case <-expiry.C:
			_ = conn.Close(CloseSessionExpired, "session expired")
			return
		case <-c.kicked:
			_ = conn.Close(CloseTooSlow, "too slow")
			return
		case <-c.down:
			_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
			return
		case <-ctx.Done():
			return
		}
	}
}
