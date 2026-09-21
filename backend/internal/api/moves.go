package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
)

// moveDTO is one thing a person may try to do to a ticket right now. The server decides what is offered, so
// clients never hold a copy of the lifecycle's rules: they show these, and say why one is blocked.
type moveDTO struct {
	To      string `json:"to"`
	Verb    string `json:"verb"`
	Primary bool   `json:"primary"`
	Blocked string `json:"blocked,omitempty"` // why it is not available at the moment
}

const cannotCheck = "This move is blocked because its rule could not be checked. Ask an administrator to review the app that defines it."

func toExprRecord(d ticketDTO) *expr.Record {
	r := &expr.Record{
		ID: d.ID.String(), Type: d.Type, Number: d.Number, Reference: d.Reference, Title: d.Title, Description: d.Description,
		Resolution: d.Resolution, Status: d.Status,
		StateCategory: d.StateCategory, Priority: int64(d.Priority), RequesterID: d.RequesterID.String(),
		RequesterName: d.Requester, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, AssigneeName: d.Assignee,
	}
	if d.AssigneeID != nil {
		id := d.AssigneeID.String()
		r.AssigneeID = &id
	}
	return r
}

// guardInput is what a transition guard sees: the ticket as it would be after the move, as it is now, and who is acting.
func guardInput(cur ticketDTO, to string, lc *apps.Lifecycle, u dbgen.User) expr.Input {
	next := cur
	next.Status = to
	if c, ok := lc.Category(to); ok {
		next.StateCategory = string(c)
	}
	return expr.Input{
		Record: toExprRecord(next), Old: toExprRecord(cur), Now: time.Now(),
		User: &expr.User{ID: u.ID.String(), Name: u.Name, Email: u.Email, Role: u.Role},
	}
}

// movesFor lists the moves offered on a ticket. Only staff can move tickets, so everyone else gets none.
func (h *handlers) movesFor(ctx context.Context, q *dbgen.Queries, p auth.Principal, cur ticketDTO) []moveDTO {
	if !isStaff(p.Role) {
		return nil
	}
	lc := h.lifecycleFor(cur.Type)
	if !lc.HasState(cur.Status) {
		// Stranded: the ticket's status is not one this lifecycle has (its app was replaced or removed).
		var out []moveDTO
		for _, t := range lc.StrandedTransitions() {
			out = append(out, moveDTO{To: t.To, Verb: t.Verb, Primary: t.Primary})
		}
		return out
	}
	var user dbgen.User
	loaded := false
	var out []moveDTO
	for _, t := range lc.TransitionsFrom(cur.Status) {
		m := moveDTO{To: t.To, Verb: t.Verb, Primary: t.Primary}
		if t.When != "" {
			if !loaded { // the acting user is only needed when some move here has a guard
				u, err := q.GetUser(ctx, p.UserID)
				if err != nil {
					slog.Error("loading the acting user for move guards", "err", err)
					m.Blocked = cannotCheck
					out = append(out, m)
					continue
				}
				user, loaded = u, true
			}
			ok, msg, err := lc.Check(ctx, cur.Status, t.To, guardInput(cur, t.To, lc, user))
			switch {
			case err != nil:
				slog.Error("move guard", "ticket", cur.Number, "from", cur.Status, "to", t.To, "err", err)
				m.Blocked = cannotCheck
			case !ok:
				m.Blocked = msg
			}
		}
		out = append(out, m)
	}
	return out
}
