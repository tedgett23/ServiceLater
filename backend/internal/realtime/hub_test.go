package realtime

import (
	"testing"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/authz"
)

func TestSlowClientIsKickedNotBlocking(t *testing.T) {
	h := NewHub()
	slow := &client{userID: uuid.New(), send: make(chan []byte, 1), kicked: make(chan struct{}), down: make(chan struct{})}
	fast := &client{userID: uuid.New(), send: make(chan []byte, 8), kicked: make(chan struct{}), down: make(chan struct{})}
	h.clients[slow], h.clients[fast] = struct{}{}, struct{}{}

	all := func(*client) bool { return true }
	h.broadcast([]byte("1"), all)
	h.broadcast([]byte("2"), all) // slow's buffer of 1 is full: it must be dropped, and broadcast must not block

	select {
	case <-slow.kicked:
	default:
		t.Error("slow client was not kicked")
	}
	select {
	case <-fast.kicked:
		t.Error("healthy client was kicked")
	default:
	}
	if len(fast.send) != 2 {
		t.Errorf("healthy client got %d messages, want 2", len(fast.send))
	}
}

func TestVisibleTo(t *testing.T) {
	me, other := uuid.New(), uuid.New()
	if !visibleTo(&client{userID: me}, "change", me) {
		t.Error("requester should see their own ticket, whatever its type")
	}
	if visibleTo(&client{userID: me}, "incident", other) {
		t.Error("end user must not see another user's ticket")
	}
	if !visibleTo(&client{userID: me, perms: authz.Set{"records:read:*"}}, "change", other) {
		t.Error("records:read:* sees every ticket")
	}
	changes := &client{userID: me, perms: authz.Set{"records:read:change"}}
	if !visibleTo(changes, "change", other) || visibleTo(changes, "incident", other) {
		t.Error("records:read:change sees changes, and only changes")
	}
}
