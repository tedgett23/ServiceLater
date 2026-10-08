// Package realtime fans Postgres LISTEN/NOTIFY changes out to connected WebSocket clients (§5.4).
//
// Events are invalidation hints that carry ids only. Clients refetch through the REST API, where RLS decides
// what they may read, so a socket can never leak ticket content, and a routing mistake costs a wasted refetch.
package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db"
)

const (
	channelTickets       = "ticket_changes"
	channelComments      = "comment_changes"
	channelNotifications = "notification_changes"
	channelFreezes       = "freeze_changes"
	channelCIs           = "ci_changes"

	sendBuffer = 32 // per client; a client that falls this far behind is dropped and resyncs on reconnect
)

// Wire events sent to clients.
type ticketEvent struct {
	Type   string    `json:"type"` // ticket.changed
	ID     uuid.UUID `json:"id"`
	Number int64     `json:"number"`
}

type commentEvent struct {
	Type     string    `json:"type"` // comment.added
	TicketID uuid.UUID `json:"ticket_id"`
}

type notificationEvent struct {
	Type string `json:"type"` // notification.changed
}

type resyncEvent struct {
	Type string `json:"type"` // resync: notifications may have been missed, refetch what you show
}

// freezeEvent says a change freeze was declared, changed or lifted (migration 00024). Everyone may read freezes, so
// it goes to everyone.
type freezeEvent struct {
	Type string    `json:"type"` // freeze.changed
	ID   uuid.UUID `json:"id"`
}

// ciEvent says a configuration item, or one of its relationships, changed (migration 00027). It goes to those who
// may read the CMDB.
type ciEvent struct {
	Type string    `json:"type"` // ci.changed
	ID   uuid.UUID `json:"id"`
}

// Notification payloads produced by the triggers in migrations 00003, 00005 and 00006.
type ticketNote struct {
	ID          uuid.UUID `json:"id"`
	Type        string    `json:"type"`
	Number      int64     `json:"number"`
	RequesterID uuid.UUID `json:"requester_id"`
}

type commentNote struct {
	TicketID    uuid.UUID  `json:"ticket_id"`
	Type        string     `json:"type"`
	Internal    bool       `json:"internal"`
	RequesterID *uuid.UUID `json:"requester_id"`
}

// Produced by the notification triggers in migration 00015: who to tell, never what happened, so a client always
// refetches its own list through /notifications rather than trusting the wire.
type notificationNote struct {
	UserID uuid.UUID `json:"user_id"`
}

type Hub struct {
	// MaxPerUser is how many live connections one person may hold (open tabs and devices). Zero means the default.
	MaxPerUser int

	mu      sync.Mutex
	clients map[*client]struct{}
	perUser map[uuid.UUID]int
	ready   chan struct{}
	once    sync.Once
}

func (h *Hub) max() int {
	if h.MaxPerUser > 0 {
		return h.MaxPerUser
	}
	return DefaultMaxConnsPerUser
}

func NewHub() *Hub {
	return &Hub{clients: map[*client]struct{}{}, perUser: map[uuid.UUID]int{}, ready: make(chan struct{})}
}

// Ready is closed once the hub is listening for notifications for the first time.
func (h *Hub) Ready() <-chan struct{} { return h.ready }

// Run listens until ctx ends, reconnecting with backoff. After every reconnect it tells all clients to resync,
// because NOTIFY is fire-and-forget and anything sent while we were away is gone.
func (h *Hub) Run(ctx context.Context, dsn string) {
	defer h.closeAll()
	db.Listen(ctx, dsn, []string{channelTickets, channelComments, channelNotifications, channelFreezes, channelCIs},
		func(reconnected bool) {
			if reconnected {
				h.broadcast(mustJSON(resyncEvent{Type: "resync"}), func(*client) bool { return true })
			}
			h.once.Do(func() { close(h.ready) })
		},
		func(channel, payload string) {
			switch channel {
			case channelTickets:
				h.onTicket(payload)
			case channelComments:
				h.onComment(payload)
			case channelNotifications:
				h.onNotification(payload)
			case channelFreezes:
				h.onFreeze(payload)
			case channelCIs:
				h.onCI(payload)
			}
		})
}

func (h *Hub) onFreeze(payload string) {
	var n struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal([]byte(payload), &n); err != nil {
		slog.Error("realtime: bad freeze notification", "err", err)
		return
	}
	h.broadcast(mustJSON(freezeEvent{Type: "freeze.changed", ID: n.ID}), func(*client) bool { return true })
}

func (h *Hub) onCI(payload string) {
	var n struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal([]byte(payload), &n); err != nil {
		slog.Error("realtime: bad configuration item notification", "err", err)
		return
	}
	h.broadcast(mustJSON(ciEvent{Type: "ci.changed", ID: n.ID}), func(c *client) bool { return c.perms.Has(authz.CMDBRead) })
}

// DefinitionsChanged tells every connected client that installed apps changed, so screens that show
// app-defined labels and flows can refetch them.
func (h *Hub) DefinitionsChanged() {
	h.broadcast(mustJSON(resyncEvent{Type: "definitions.changed"}), func(*client) bool { return true })
}

// Routing mirrors the tasks_select policy: whoever has records:read for the record's type (or every type) sees it,
// everyone else only their own. It is only a routing hint; the data itself is always read through RLS.
func visibleTo(c *client, recordType string, requester uuid.UUID) bool {
	return c.perms.CanRecord(authz.Read, recordType) || c.userID == requester
}

func (h *Hub) onTicket(payload string) {
	var n ticketNote
	if err := json.Unmarshal([]byte(payload), &n); err != nil {
		slog.Error("realtime: bad ticket notification", "err", err)
		return
	}
	h.broadcast(mustJSON(ticketEvent{Type: "ticket.changed", ID: n.ID, Number: n.Number}),
		func(c *client) bool { return visibleTo(c, n.Type, n.RequesterID) })
}

func (h *Hub) onComment(payload string) {
	var n commentNote
	if err := json.Unmarshal([]byte(payload), &n); err != nil || n.RequesterID == nil {
		slog.Error("realtime: bad comment notification", "err", err)
		return
	}
	h.broadcast(mustJSON(commentEvent{Type: "comment.added", TicketID: n.TicketID}), func(c *client) bool {
		if n.Internal && !c.internal {
			return false // even the existence of an internal note is for those who may read them
		}
		return visibleTo(c, n.Type, *n.RequesterID)
	})
}

// onNotification tells exactly the one person a notification was created or cleared for, so an approver's
// header updates the moment a workflow asks for their decision, and clears the moment they (or someone else, for a
// multi-approver step) decide it. There is no broader audience to reason about, unlike a ticket or comment.
func (h *Hub) onNotification(payload string) {
	var n notificationNote
	if err := json.Unmarshal([]byte(payload), &n); err != nil {
		slog.Error("realtime: bad notification", "err", err)
		return
	}
	h.broadcast(mustJSON(notificationEvent{Type: "notification.changed"}), func(c *client) bool { return c.userID == n.UserID })
}

// broadcast queues msg for every allowed client without ever blocking on a slow one.
func (h *Hub) broadcast(msg []byte, allow func(*client) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if !allow(c) {
			continue
		}
		select {
		case c.send <- msg:
		default:
			c.kick() // too slow: drop it, the client reconnects and resyncs
		}
	}
}

func (h *Hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		c.shutdown()
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // the event types above cannot fail to marshal
	}
	return b
}
