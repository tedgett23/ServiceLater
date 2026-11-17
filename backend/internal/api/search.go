package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

const (
	// searchMinLength is how much someone must type before a search runs: three characters, which is also the length
	// of most reference prefixes (INC, CHG, REQ).
	searchMinLength = 3
	searchMaxLength = 100
	// searchPerKind is how many results of each kind a search returns: it is a way to jump, not a list to page.
	searchPerKind = 8
	// articlePrefix is the prefix of a knowledge article's reference (KB12).
	articlePrefix = "KB"
)

// searchResultDTO is one thing a search found, ready to show and to open.
type searchResultDTO struct {
	// Kind is record, article, item or person.
	Kind string `json:"kind"`
	ID   string `json:"id"`
	// Reference is how people name it (INC42, KB7); empty for items and people.
	Reference string `json:"reference,omitempty"`
	Title     string `json:"title"`
	// Detail is the small print beside it: a record's type and status, an item's class, a person's email.
	Detail string `json:"detail,omitempty"`
	// Link is the page in the web UI that shows it.
	Link string `json:"link"`
}

type searchDTO struct {
	Query string `json:"query"`
	// Mode says how the query was read: reference when it began with a record type's prefix (INC, INC42) and that
	// found something, else text (the words, typos allowed).
	Mode string `json:"mode"`
	// Prefix is the reference prefix the query began with, in reference mode.
	Prefix string            `json:"prefix,omitempty"`
	Items  []searchResultDTO `json:"items"`
}

// A reference as someone types it: letters, then perhaps a number (inc, INC42, inc 42, INC-42).
var searchRefRE = regexp.MustCompile(`^([A-Za-z]{2,5})[\s-]?([0-9]{0,15})$`)

// GET /search?q=: jump to a record, a knowledge article, a configuration item or (for those who manage people) a
// person. q must be at least three characters. If it begins with a record type's prefix it is read as a reference
// first: INC lists the latest incidents, INC4 the incidents whose number starts with 4, exact match first. When that
// finds nothing, or q is not a reference, it is searched as text: titles and names that contain it, then those that
// look like it (typos, parts of words), and the text of articles. Everything is searched as the caller, so it finds
// only what they may see; at most eight of each kind.
func (h *handlers) search(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if n := utf8.RuneCountInString(query); n < searchMinLength || n > searchMaxLength {
		writeError(w, http.StatusBadRequest, "q must be 3 to 100 characters")
		return
	}
	out := searchDTO{Query: query, Mode: "text", Items: []searchResultDTO{}}
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		ctx := r.Context()
		if m := searchRefRE.FindStringSubmatch(query); m != nil {
			items, err := h.searchReference(r, q, strings.ToUpper(m[1]), m[2])
			if err != nil {
				return err
			}
			if len(items) > 0 {
				out.Mode, out.Prefix, out.Items = "reference", strings.ToUpper(m[1]), items
				return nil
			}
		}

		contains := likeEscape(query)
		records, err := q.SearchRecords(ctx, dbgen.SearchRecordsParams{Contains: contains, Q: query, Limit: searchPerKind})
		if err != nil {
			return err
		}
		for _, t := range records {
			out.Items = append(out.Items, h.recordResult(t.ID.String(), t.Type, t.TypeLabel, t.Reference, t.Title, t.Status))
		}
		articles, err := q.SearchArticlesAnywhere(ctx, dbgen.SearchArticlesAnywhereParams{Contains: contains, Q: query, Limit: searchPerKind})
		if err != nil {
			return err
		}
		for _, a := range articles {
			out.Items = append(out.Items, articleResult(a.ID.String(), a.Number, a.Title, a.Status))
		}
		// RLS already hides items from those who do not read the CMDB; asking saves the query.
		if p.Can(authz.CMDBRead) {
			cis, err := q.SearchCIs(ctx, dbgen.SearchCIsParams{Contains: contains, Q: query, Limit: searchPerKind})
			if err != nil {
				return err
			}
			for _, c := range cis {
				detail := c.Class
				if cl := h.apps.Class(c.Class); cl != nil {
					detail = cl.Label
				}
				out.Items = append(out.Items, searchResultDTO{Kind: "item", ID: c.ID.String(), Title: c.Name, Detail: detail, Link: "/cmdb/" + c.ID.String()})
			}
		}
		// People have a page only in the admin section, so only those who manage them find them here.
		if p.Can(authz.UsersManage) {
			people, err := q.ListUsers(ctx, dbgen.ListUsersParams{Q: pgtype.Text{String: contains, Valid: true}, Limit: searchPerKind})
			if err != nil {
				return err
			}
			for _, u := range people {
				out.Items = append(out.Items, searchResultDTO{Kind: "person", ID: u.ID.String(), Title: u.Name, Detail: u.Email, Link: "/admin/users/" + u.ID.String()})
			}
		}
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// searchReference reads a query as a reference: a prefix in use (a record type's, or KB) and the digits typed so
// far. It returns nothing when the letters are not a prefix, so the caller searches them as text.
func (h *handlers) searchReference(r *http.Request, q *dbgen.Queries, prefix, digits string) ([]searchResultDTO, error) {
	ctx := r.Context()
	digits = strings.TrimLeft(digits, "0") // INC007 is INC7
	var out []searchResultDTO
	if prefix == articlePrefix {
		rows, err := q.SearchArticlesByNumber(ctx, dbgen.SearchArticlesByNumberParams{Digits: digits, Limit: searchPerKind})
		if err != nil {
			return nil, err
		}
		for _, a := range rows {
			out = append(out, articleResult(a.ID.String(), a.Number, a.Title, a.Status))
		}
		return out, nil
	}
	tt, err := q.GetLiveTaskTypeByPrefix(ctx, prefix)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if digits == "" {
		rows, err := q.LatestRecordsOfType(ctx, dbgen.LatestRecordsOfTypeParams{Type: tt.Type, Limit: searchPerKind})
		if err != nil {
			return nil, err
		}
		for _, t := range rows {
			out = append(out, h.recordResult(t.ID.String(), t.Type, t.TypeLabel, t.Reference, t.Title, t.Status))
		}
		return out, nil
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || len(digits) > 12 {
		return nil, nil
	}
	rows, err := q.SearchRecordsByNumber(ctx, dbgen.SearchRecordsByNumberParams{Type: tt.Type, Digits: n, Limit: searchPerKind})
	if err != nil {
		return nil, err
	}
	for _, t := range rows {
		out = append(out, h.recordResult(t.ID.String(), t.Type, t.TypeLabel, t.Reference, t.Title, t.Status))
	}
	return out, nil
}

func (h *handlers) recordResult(id, recordType, typeLabel, reference, title, status string) searchResultDTO {
	return searchResultDTO{Kind: "record", ID: id, Reference: reference, Title: title,
		Detail: typeLabel + " · " + h.lifecycleFor(recordType).Label(status), Link: "/tickets/" + reference}
}

func articleResult(id string, number int64, title, status string) searchResultDTO {
	detail := "Article"
	if status != "published" {
		detail += " · " + status
	}
	return searchResultDTO{Kind: "article", ID: id, Reference: articleRef(number), Title: title, Detail: detail, Link: "/kb/" + articleRef(number)}
}
