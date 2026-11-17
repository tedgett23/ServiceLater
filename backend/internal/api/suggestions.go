package api

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
)

const (
	// suggestThreshold is how alike a record must be to be suggested (app_match_score, plus 0.5 for a shared item):
	// one distinctive word in three in common. "VPN disconnects all the time" and "VPN concentrator drops idle
	// sessions" score 0.33, "Mail is slow" and "Mail store is full" 0.5; "VPN drops every 20 minutes" and "VPN
	// certificate renewals fail" (0.25) and "Printer offline" and "VPN drops" (0) are left out.
	suggestThreshold     = 0.3
	maxRecordSuggestions = 5
)

// suggestionDTO is a record suggested as related, with why.
type suggestionDTO struct {
	childTaskDTO
	// Fields are its extra fields, so a known error's workaround can be shown where it is suggested.
	Fields map[string]any `json:"fields"`
	// Why says what makes it look related: "Same affected item" or "Similar title".
	Why string `json:"why"`
}

// suggest runs SuggestRecords and keeps the best few that are alike enough.
func suggest(r *http.Request, q *dbgen.Queries, arg dbgen.SuggestRecordsParams) ([]suggestionDTO, error) {
	if arg.Cis == nil {
		arg.Cis = []uuid.UUID{}
	}
	rows, err := q.SuggestRecords(r.Context(), arg)
	out := []suggestionDTO{}
	for _, row := range rows {
		if row.Score < suggestThreshold || len(out) == maxRecordSuggestions {
			break
		}
		why := "Similar title"
		if row.SharedItem {
			why = "Same affected item"
		}
		out = append(out, suggestionDTO{childTaskDTO: summaryOf(row.Task, row.Reference, row.AssigneeName, row.AssignmentGroupName),
			Fields: records.DecodeFields(row.Task.Fields), Why: why})
	}
	return out, err
}

// GET /tickets/{id}/suggestions: records that look related to this one and are not linked to it yet. For an incident
// not linked to a problem, the open problems that may be behind it; for a problem, the open incidents not linked to
// any problem that it may explain. A record looks related when its title is alike, or it is linked to one of the same
// configuration items, which counts for more. Other types get none. Only what the caller may see is suggested.
func (h *handlers) ticketSuggestions(w http.ResponseWriter, r *http.Request) {
	out := []suggestionDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		row, err := loadTicket(r, q)
		if err != nil {
			return err
		}
		t, err := records.Load(r.Context(), q, row.ID)
		if err != nil {
			return err
		}
		arg := dbgen.SuggestRecordsParams{Text: t.Title, Exclude: t.ID}
		for _, c := range t.CIs {
			arg.Cis = append(arg.Cis, c.ID)
		}
		switch {
		case t.Type == "incident" && t.ProblemID == nil:
			arg.Type = "problem"
		case t.Type == "problem" && t.StateCategory != "closed":
			arg.Type, arg.Unlinked = "incident", true
		default:
			return nil
		}
		out, err = suggest(r, q, arg)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// GET /tickets/suggest?type=&q=: open records of a type whose titles look like q (3 to 200 characters), for a
// record being filed: the New ticket dialog asks for problems like the incident being typed, so a known error and
// its workaround show before it is filed. Only what the caller may see is suggested.
func (h *handlers) suggestTickets(w http.ResponseWriter, r *http.Request) {
	text := strings.TrimSpace(r.URL.Query().Get("q"))
	typ := r.URL.Query().Get("type")
	if n := utf8.RuneCountInString(text); n < 3 || n > 200 {
		writeError(w, http.StatusBadRequest, "q must be 3 to 200 characters")
		return
	}
	if typ == "" || typ == "task" || !h.apps.KnownType(typ) {
		writeError(w, http.StatusBadRequest, "unknown type")
		return
	}
	out := []suggestionDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		out, err = suggest(r, q, dbgen.SuggestRecordsParams{Text: text, Type: typ})
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
