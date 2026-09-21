package api

import (
	"context"
	"log/slog"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
)

// moveDTO is one thing a person may try to do to a ticket right now. The server decides what is offered, so
// clients never hold a copy of the lifecycle's rules: they show these, and say why one is blocked.
type moveDTO struct {
	To      string `json:"to"`
	Verb    string `json:"verb"`
	Primary bool   `json:"primary"`
	Blocked string `json:"blocked,omitempty"` // why it is not available at the moment
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
					m.Blocked = records.CannotCheck
					out = append(out, m)
					continue
				}
				user, loaded = u, true
			}
			ok, msg, err := lc.Check(ctx, cur.Status, t.To, records.GuardInput(cur.Ticket, t.To, lc, user))
			switch {
			case err != nil:
				slog.Error("move guard", "ticket", cur.Number, "from", cur.Status, "to", t.To, "err", err)
				m.Blocked = records.CannotCheck
			case !ok:
				m.Blocked = msg
			}
		}
		out = append(out, m)
	}
	return out
}
