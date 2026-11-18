// Package realtime fans Postgres LISTEN/NOTIFY changes out to connected WebSocket clients (§5.4).
//
// Events are invalidation hints that carry ids only. Clients refetch through the REST API, where RLS decides
// what they may read, so a socket can never leak ticket content, and a routing mistake costs a wasted refetch.
package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

const (
	channelTickets       = "ticket_changes"
	channelComments      = "comment_changes"
	channelNotifications = "notification_changes"
	channelFreezes       = "freeze_changes"
	channelCIs           = "ci_changes"
	channelEmail         = "email_changes"
	channelRuns          = "run_changes"

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

// freezeEvent says a change freeze was declared, changed or lifted (migrations/00011_freezes_slas.sql). Everyone may read freezes, so
// it goes to everyone.
type freezeEvent struct {
	Type string    `json:"type"` // freeze.changed
	ID   uuid.UUID `json:"id"`
}

// ciEvent says a configuration item, or one of its relationships, changed (migrations/00010_cmdb.sql). It goes to those who
// may read the CMDB. Bulk (an import, app.ci_bulk) means many items changed at once and carries no id: refetch
// whatever is shown.
type ciEvent struct {
	Type string    `json:"type"` // ci.changed
	ID   uuid.UUID `json:"id"`
	Bulk bool      `json:"bulk,omitempty"`
}

// Notification payloads produced by the triggers in migrations/00007_records.sql.
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

// Produced by the notification triggers in migrations/00009_notifications.sql: who to tell, never what happened, so a client always
// refetches its own list through /notifications rather than trusting the wire.
type notificationNote struct {
	UserID uuid.UUID `json:"user_id"`
}

type Hub struct {
	// MaxPerUser is how many live connections one person may hold (open tabs and devices). Zero means the default.
	MaxPerUser int
	// Audience says whether a record is routed to a restricted group and, if so, who besides those holding
	// restricted:read may hear about it (the group's members and the assignee). Nil treats every record as
	// unrestricted (tests that do not need it).
	Audience func(ctx context.Context, taskID uuid.UUID) (Audience, error)

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
	db.Listen(ctx, dsn, []string{channelTickets, channelComments, channelNotifications, channelFreezes, channelCIs, channelEmail, channelRuns},
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
			case channelRuns:
				h.onRun(payload)
			case channelEmail:
				// An email was queued, sent or failed: the delivery log is for those who manage settings.
				h.broadcast(mustJSON(resyncEvent{Type: "email.changed"}), func(c *client) bool { return c.perms.Has(authz.SettingsManage) })
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
		ID   uuid.UUID `json:"id"`
		Bulk bool      `json:"bulk"`
	}
	if err := json.Unmarshal([]byte(payload), &n); err != nil {
		slog.Error("realtime: bad configuration item notification", "err", err)
		return
	}
	h.broadcast(mustJSON(ciEvent{Type: "ci.changed", ID: n.ID, Bulk: n.Bulk}), func(c *client) bool { return c.perms.Has(authz.CMDBRead) })
}

// runEvent says a workflow run started, moved to another step or ended (migrations/00008_workflows.sql). It goes to those who see
// every run (the builder and the runs page), with the run's id and its workflow's name only.
type runEvent struct {
	Type     string    `json:"type"` // run.changed
	ID       uuid.UUID `json:"id"`
	App      string    `json:"app"`
	Workflow string    `json:"workflow"`
}

func (h *Hub) onRun(payload string) {
	var n struct {
		ID       uuid.UUID `json:"id"`
		App      string    `json:"app"`
		Workflow string    `json:"workflow"`
	}
	if err := json.Unmarshal([]byte(payload), &n); err != nil {
		slog.Error("realtime: bad run notification", "err", err)
		return
	}
	h.broadcast(mustJSON(runEvent{Type: "run.changed", ID: n.ID, App: n.App, Workflow: n.Workflow}),
		func(c *client) bool { return c.perms.Has(authz.WorkflowsReadAll) })
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

// Audience is who may hear about a record routed to a restricted group.
type Audience struct {
	Restricted bool
	Allowed    map[uuid.UUID]bool // the group's members and the assignee
}

// recordAudience is visibleTo plus restricted groups, mirroring tasks_select (app_sees_task): a record routed to a
// restricted group is heard of only by its requester and, among those who may read its type, the group's members,
// its assignee and those who hold restricted:read. If the audience cannot be looked up, only the requester hears.
func (h *Hub) recordAudience(taskID uuid.UUID, recordType string, requester uuid.UUID) func(*client) bool {
	base := func(c *client) bool { return visibleTo(c, recordType, requester) }
	if h.Audience == nil {
		return base
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := h.Audience(ctx, taskID)
	if err != nil {
		slog.Error("realtime: cannot tell who may hear about a record; telling only its requester", "record", taskID, "err", err)
		return func(c *client) bool { return c.userID == requester }
	}
	if !a.Restricted {
		return base
	}
	return func(c *client) bool {
		return c.userID == requester ||
			(c.perms.CanRecord(authz.Read, recordType) && (a.Allowed[c.userID] || c.perms.Has(authz.RestrictedRead)))
	}
}

func (h *Hub) onTicket(payload string) {
	var n ticketNote
	if err := json.Unmarshal([]byte(payload), &n); err != nil {
		slog.Error("realtime: bad ticket notification", "err", err)
		return
	}
	h.broadcast(mustJSON(ticketEvent{Type: "ticket.changed", ID: n.ID, Number: n.Number}), h.recordAudience(n.ID, n.Type, n.RequesterID))
}

func (h *Hub) onComment(payload string) {
	var n commentNote
	if err := json.Unmarshal([]byte(payload), &n); err != nil || n.RequesterID == nil {
		slog.Error("realtime: bad comment notification", "err", err)
		return
	}
	sees := h.recordAudience(n.TicketID, n.Type, *n.RequesterID)
	h.broadcast(mustJSON(commentEvent{Type: "comment.added", TicketID: n.TicketID}), func(c *client) bool {
		if n.Internal && !c.internal {
			return false // even the existence of an internal note is for those who may read them
		}
		return sees(c)
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

// AudienceFrom looks a record's audience up in the database, as the system: the hub routes hints, and what the
// client then fetches is still checked by RLS.
func AudienceFrom(pool *pgxpool.Pool) func(context.Context, uuid.UUID) (Audience, error) {
	return func(ctx context.Context, taskID uuid.UUID) (Audience, error) {
		var out Audience
		err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
			row, err := dbgen.New(tx).TaskAudience(ctx, taskID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // gone (deleted): nothing to hide
			}
			if err != nil || !row.Restricted {
				return err
			}
			out.Restricted = true
			out.Allowed = map[uuid.UUID]bool{}
			for _, m := range row.Members {
				out.Allowed[m] = true
			}
			if row.AssigneeID.Valid {
				out.Allowed[row.AssigneeID.UUID] = true
			}
			return nil
		})
		return out, err
	}
}
