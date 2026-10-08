package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

const (
	maxArticleTitle    = 200
	maxArticleBody     = 100000
	maxArticleCategory = 100
	maxArticleTags     = 20
	maxArticleTag      = 40
	maxSuggestions     = 5
)

// Article statuses. A writer drafts one and sends it for review; one who publishes publishes it, and later retires it.
const (
	articleDraft     = "draft"
	articleReview    = "review"
	articlePublished = "published"
	articleRetired   = "retired"
)

// articleDTO is a knowledge article.
type articleDTO struct {
	ID uuid.UUID `json:"id"`
	// Reference names it, such as KB12.
	Reference string `json:"reference"`
	Number    int64  `json:"number"`
	Title     string `json:"title"`
	// Body is Markdown, as written. Render it with a sanitiser: it is never HTML to trust.
	Body     string   `json:"body"`
	Category string   `json:"category"`
	Tags     []string `json:"tags"`
	// Audience is everyone (anyone signed in) or internal (staff, with knowledge:read_internal).
	Audience string `json:"audience"`
	// Status is draft, review, published or retired.
	Status string `json:"status"`
	// Version counts the saves of its content, from 1.
	Version       int32      `json:"version"`
	AuthorName    *string    `json:"author_name"`
	UpdatedByName *string    `json:"updated_by_name"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	PublishedAt   *time.Time `json:"published_at"`
	// Moves are the status changes the reader may make now (single-article responses).
	Moves []string `json:"moves,omitempty"`
	// Tickets are the records linked to it that the reader may see (single-article responses, for staff).
	Tickets []articleTicketDTO `json:"tickets,omitempty"`
}

// articleTicketDTO is a record an article is linked to.
type articleTicketDTO struct {
	ID            uuid.UUID `json:"id"`
	Reference     string    `json:"reference"`
	Type          string    `json:"type"`
	Title         string    `json:"title"`
	Status        string    `json:"status"`
	StateCategory string    `json:"state_category"`
}

// articleHitDTO is an article in a list or search result.
type articleHitDTO struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	Title     string    `json:"title"`
	Category  string    `json:"category"`
	Tags      []string  `json:"tags"`
	Audience  string    `json:"audience"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
	// Snippet is a piece of the body: around the matches when searching, the start otherwise. Matches are between
	// U+2E22 and U+2E23; it is plain text, never HTML.
	Snippet string `json:"snippet"`
}

// articleRefDTO is an article linked to a record.
type articleRefDTO struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Audience  string    `json:"audience"`
}

func articleRef(n int64) string { return fmt.Sprintf("KB%d", n) }

func toArticle(a dbgen.GetArticleRow) articleDTO {
	out := articleDTO{ID: a.ID, Reference: articleRef(a.Number), Number: a.Number, Title: a.Title, Body: a.Body, Category: a.Category,
		Tags: a.Tags, Audience: a.Audience, Status: a.Status, Version: a.Version, AuthorName: textPtr(a.AuthorName),
		UpdatedByName: textPtr(a.UpdatedByName), CreatedAt: a.CreatedAt.Time, UpdatedAt: a.UpdatedAt.Time}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	if a.PublishedAt.Valid {
		out.PublishedAt = &a.PublishedAt.Time
	}
	return out
}

var articleRefRE = regexp.MustCompile(`^(?i:KB)?([0-9]{1,18})$`)

// loadArticle resolves the {id} path segment: an id, a reference (KB12) or a number.
func loadArticle(r *http.Request, q *dbgen.Queries) (dbgen.GetArticleRow, error) {
	key := chi.URLParam(r, "id")
	id, err := uuid.Parse(key)
	if err != nil {
		m := articleRefRE.FindStringSubmatch(key)
		if m == nil {
			return dbgen.GetArticleRow{}, notFound()
		}
		n, _ := strconv.ParseInt(m[1], 10, 64)
		if id, err = q.GetArticleIDByNumber(r.Context(), n); err != nil {
			return dbgen.GetArticleRow{}, err
		}
	}
	return q.GetArticle(r.Context(), id)
}

// articleMoves are the status changes this person may make to an article in this status.
func articleMoves(p auth.Principal, status string) []string {
	write, publish := p.Can(authz.KnowledgeWrite), p.Can(authz.KnowledgePublish)
	var out []string
	switch status {
	case articleDraft:
		if write || publish {
			out = append(out, articleReview)
		}
		if publish {
			out = append(out, articlePublished)
		}
	case articleReview:
		if publish {
			out = append(out, articlePublished)
		}
		if write || publish {
			out = append(out, articleDraft)
		}
	case articlePublished:
		if publish {
			out = append(out, articleRetired)
		}
	case articleRetired:
		if publish {
			out = append(out, articlePublished, articleDraft)
		}
	}
	return out
}

// articleRequest is an article's content as it is written or changed; on PATCH, a field left out keeps its value.
type articleRequest struct {
	Title    *string   `json:"title"`
	Body     *string   `json:"body"`
	Category *string   `json:"category"`
	Tags     *[]string `json:"tags"`
	// Audience is everyone or internal.
	Audience *string `json:"audience"`
}

type articleContent struct {
	title, body, category, audience string
	tags                            []string
}

func (req articleRequest) apply(c articleContent) (articleContent, error) {
	if req.Title != nil {
		c.title = strings.TrimSpace(*req.Title)
	}
	if req.Body != nil {
		c.body = *req.Body
	}
	if req.Category != nil {
		c.category = strings.Join(strings.Fields(*req.Category), " ")
	}
	if req.Audience != nil {
		c.audience = *req.Audience
	}
	if req.Tags != nil {
		c.tags = []string{}
		for _, t := range *req.Tags {
			t = strings.ToLower(strings.Join(strings.Fields(t), " "))
			if t != "" && !slices.Contains(c.tags, t) {
				c.tags = append(c.tags, t)
			}
		}
	}
	switch n := utf8.RuneCountInString(c.title); {
	case n < 1 || n > maxArticleTitle:
		return c, invalid(fmt.Sprintf("title must be 1 to %d characters", maxArticleTitle))
	case utf8.RuneCountInString(c.body) > maxArticleBody:
		return c, invalid(fmt.Sprintf("the article must be at most %d characters", maxArticleBody))
	case utf8.RuneCountInString(c.category) > maxArticleCategory:
		return c, invalid(fmt.Sprintf("category must be at most %d characters", maxArticleCategory))
	case c.audience != "everyone" && c.audience != "internal":
		return c, invalid("audience must be everyone or internal")
	case len(c.tags) > maxArticleTags:
		return c, invalid(fmt.Sprintf("an article may have at most %d tags", maxArticleTags))
	}
	for _, t := range c.tags {
		if utf8.RuneCountInString(t) > maxArticleTag {
			return c, invalid(fmt.Sprintf("a tag must be at most %d characters", maxArticleTag))
		}
	}
	return c, nil
}

// GET /kb/articles?q=&category=&status=&limit=&offset=: knowledge articles the caller may read, best match first when
// q (words, "a phrase", -word) is given, latest changed otherwise. status is published unless the caller writes
// articles, who may ask for draft, review, retired or all. more says there are more after this page.
func (h *handlers) listArticles(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	limit, offset := defaultLimit, 0
	if !intParam(w, qs.Get("limit"), "limit", 1, maxLimit, &limit) || !intParam(w, qs.Get("offset"), "offset", 0, 1<<30, &offset) {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	statuses := []string{articlePublished}
	switch s := qs.Get("status"); s {
	case "", articlePublished:
	case articleDraft, articleReview, articleRetired, "all":
		if !p.Can(authz.KnowledgeWrite) && !p.Can(authz.KnowledgePublish) {
			writeError(w, http.StatusForbidden, "only those who write articles see ones that are not published")
			return
		}
		statuses = []string{s}
		if s == "all" {
			statuses = []string{articleDraft, articleReview, articlePublished, articleRetired}
		}
	default:
		writeError(w, http.StatusBadRequest, "status must be draft, review, published, retired or all")
		return
	}
	args := dbgen.SearchArticlesParams{Statuses: statuses, Limit: int32(limit + 1), Offset: int32(offset)}
	if s := strings.TrimSpace(qs.Get("q")); s != "" {
		args.Q = pgtype.Text{String: s, Valid: true}
	}
	if c := strings.TrimSpace(qs.Get("category")); c != "" {
		args.Category = pgtype.Text{String: c, Valid: true}
	}
	out := []articleHitDTO{}
	more := false
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.SearchArticles(r.Context(), args)
		if len(rows) > limit {
			rows, more = rows[:limit], true
		}
		for _, a := range rows {
			out = append(out, toHit(a))
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "more": more})
}

func toHit(a dbgen.SearchArticlesRow) articleHitDTO {
	hit := articleHitDTO{ID: a.ID, Reference: articleRef(a.Number), Title: a.Title, Category: a.Category, Tags: a.Tags,
		Audience: a.Audience, Status: a.Status, UpdatedAt: a.UpdatedAt.Time, Snippet: a.Snippet}
	if hit.Tags == nil {
		hit.Tags = []string{}
	}
	return hit
}

// GET /kb/suggest?q=: up to five published articles that match what someone is writing, such as a ticket's summary
// while it is filed. Fewer than three letters suggest nothing.
func (h *handlers) suggestArticles(w http.ResponseWriter, r *http.Request) {
	text := strings.TrimSpace(r.URL.Query().Get("q"))
	out := []articleHitDTO{}
	if utf8.RuneCountInString(text) < 3 {
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
		return
	}
	// A summary is a sentence, not a search: any of its words may match, and the best matches come first.
	words := strings.FieldsFunc(strings.ToLower(text), func(c rune) bool { return !unicode.IsLetter(c) && !unicode.IsDigit(c) })
	if len(words) > 12 {
		words = words[:12]
	}
	query := strings.Join(words, " or ")
	if query == "" {
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.SearchArticles(r.Context(), dbgen.SearchArticlesParams{Q: pgtype.Text{String: query, Valid: true},
			Statuses: []string{articlePublished}, Limit: maxSuggestions})
		for _, a := range rows {
			out = append(out, toHit(a))
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// GET /kb/categories: the categories of the published articles the caller may read, with how many each has.
func (h *handlers) listArticleCategories(w http.ResponseWriter, r *http.Request) {
	type category struct {
		Name     string `json:"name"`
		Articles int64  `json:"articles"`
	}
	out := []category{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListArticleCategories(r.Context())
		for _, c := range rows {
			out = append(out, category{Name: c.Category, Articles: c.Articles})
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// GET /kb/articles/{id}: one article, by id, reference (KB12) or number, with the status changes the caller may
// make and, for staff, the records it is linked to.
func (h *handlers) getArticle(w http.ResponseWriter, r *http.Request) {
	var out articleDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		a, err := loadArticle(r, q)
		if err != nil {
			return err
		}
		out = toArticle(a)
		out.Moves = articleMoves(p, a.Status)
		if !p.Perms.CanRecordAny(authz.Read) {
			return nil
		}
		rows, err := q.ListArticleTasks(r.Context(), a.ID)
		for _, t := range rows {
			out.Tickets = append(out.Tickets, articleTicketDTO{ID: t.ID, Reference: fmt.Sprintf("%s%d", t.Prefix, t.Number),
				Type: t.Type, Title: t.Title, Status: t.Status, StateCategory: t.StateCategory})
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /kb/articles (knowledge:write) with {"title", "body"?, "category"?, "tags"?, "audience"?}: draft an article.
// Audience is everyone unless it says internal.
func (h *handlers) createArticle(w http.ResponseWriter, r *http.Request) {
	var req articleRequest
	if !decode(w, r, &req) {
		return
	}
	var out articleDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		c, err := req.apply(articleContent{audience: "everyone", tags: []string{}})
		if err != nil {
			return err
		}
		id, err := q.CreateArticle(r.Context(), dbgen.CreateArticleParams{Title: c.title, Body: c.body, Category: c.category,
			Tags: c.tags, Audience: c.audience, Status: articleDraft})
		if err != nil {
			return err
		}
		if err := q.SaveArticleVersion(r.Context(), id); err != nil {
			return err
		}
		a, err := q.GetArticle(r.Context(), id)
		out = toArticle(a)
		out.Moves = articleMoves(p, a.Status)
		return err
	}) {
		return
	}
	w.Header().Set("Location", "/api/v1/kb/articles/"+out.Reference)
	writeJSON(w, http.StatusCreated, out)
}

// PATCH /kb/articles/{id} with any of {"title", "body", "category", "tags", "audience"}: change an article's content,
// keeping the previous version. Writers change drafts and articles in review; a published or retired one only those
// who publish (and it stays published).
func (h *handlers) updateArticle(w http.ResponseWriter, r *http.Request) {
	var req articleRequest
	if !decode(w, r, &req) {
		return
	}
	var out articleDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		a, err := loadArticle(r, q)
		if err != nil {
			return err
		}
		if (a.Status == articlePublished || a.Status == articleRetired) && !p.Can(authz.KnowledgePublish) {
			return forbidden(fmt.Sprintf("a %s article is changed only by those who publish articles (%s)", a.Status, authz.KnowledgePublish))
		}
		if !p.Can(authz.KnowledgeWrite) && !p.Can(authz.KnowledgePublish) {
			return forbidden(fmt.Sprintf("changing articles needs %s", authz.KnowledgeWrite))
		}
		c, err := req.apply(articleContent{title: a.Title, body: a.Body, category: a.Category, tags: a.Tags, audience: a.Audience})
		if err != nil {
			return err
		}
		if _, err := q.UpdateArticleContent(r.Context(), dbgen.UpdateArticleContentParams{ID: a.ID, Title: c.title, Body: c.body,
			Category: c.category, Tags: c.tags, Audience: c.audience}); err != nil {
			return err
		}
		if err := q.SaveArticleVersion(r.Context(), a.ID); err != nil {
			return err
		}
		got, err := q.GetArticle(r.Context(), a.ID)
		out = toArticle(got)
		out.Moves = articleMoves(p, got.Status)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// articleStatusRequest moves an article to another status.
type articleStatusRequest struct {
	// To is review (a writer sends a draft for review), draft (back to its writer), published or retired (those who
	// publish).
	To string `json:"to"`
}

// POST /kb/articles/{id}/status with {"to"}: move an article. The moves each person may make are in moves.
func (h *handlers) setArticleStatus(w http.ResponseWriter, r *http.Request) {
	var req articleStatusRequest
	if !decode(w, r, &req) {
		return
	}
	var out articleDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		a, err := loadArticle(r, q)
		if err != nil {
			return err
		}
		if !slices.Contains(articleMoves(p, a.Status), req.To) {
			switch {
			case !p.Can(authz.KnowledgeWrite) && !p.Can(authz.KnowledgePublish):
				return forbidden(fmt.Sprintf("changing articles needs %s", authz.KnowledgeWrite))
			case !p.Can(authz.KnowledgePublish) && (req.To == articlePublished || req.To == articleRetired || a.Status == articlePublished || a.Status == articleRetired):
				return forbidden(fmt.Sprintf("publishing, retiring and reopening articles needs %s", authz.KnowledgePublish))
			}
			return invalid(fmt.Sprintf("a %s article cannot be moved to %s", a.Status, req.To))
		}
		if _, err := q.SetArticleStatus(r.Context(), dbgen.SetArticleStatusParams{ID: a.ID, Status: req.To}); err != nil {
			return err
		}
		got, err := q.GetArticle(r.Context(), a.ID)
		out = toArticle(got)
		out.Moves = articleMoves(p, got.Status)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /kb/articles/{id} (knowledge:publish): delete an article that was never published; retire one that was.
func (h *handlers) deleteArticle(w http.ResponseWriter, r *http.Request) {
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		a, err := loadArticle(r, q)
		if err != nil {
			return err
		}
		if a.PublishedAt.Valid {
			return &apiError{http.StatusConflict, "this article has been published: retire it instead"}
		}
		n, err := q.DeleteArticle(r.Context(), a.ID)
		if err == nil && n == 0 {
			return notFound()
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// articleVersionDTO is one saved version of an article; the list leaves out the content.
type articleVersionDTO struct {
	Version     int32     `json:"version"`
	Title       string    `json:"title"`
	Status      string    `json:"status"`
	SavedAt     time.Time `json:"saved_at"`
	SavedByName *string   `json:"saved_by_name"`
	Body        *string   `json:"body,omitempty"`
	Category    *string   `json:"category,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	Audience    string    `json:"audience,omitempty"`
}

// GET /kb/articles/{id}/versions: an article's saved versions, newest first (who saved each, and when).
func (h *handlers) listArticleVersions(w http.ResponseWriter, r *http.Request) {
	out := []articleVersionDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		a, err := loadArticle(r, q)
		if err != nil {
			return err
		}
		rows, err := q.ListArticleVersions(r.Context(), a.ID)
		for _, v := range rows {
			out = append(out, articleVersionDTO{Version: v.Version, Title: v.Title, Status: v.Status, SavedAt: v.SavedAt.Time,
				SavedByName: textPtr(v.SavedByName)})
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// GET /kb/articles/{id}/versions/{version}: one saved version, with its content.
func (h *handlers) getArticleVersion(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil || n < 1 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var out articleVersionDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		a, err := loadArticle(r, q)
		if err != nil {
			return err
		}
		v, err := q.GetArticleVersion(r.Context(), dbgen.GetArticleVersionParams{ArticleID: a.ID, Version: int32(n)}) //nolint:gosec // checked above
		if err != nil {
			return err
		}
		out = articleVersionDTO{Version: v.Version, Title: v.Title, Status: v.Status, SavedAt: v.SavedAt.Time,
			SavedByName: textPtr(v.SavedByName), Body: &v.Body, Category: &v.Category, Tags: v.Tags, Audience: v.Audience}
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// articlesOf are the articles linked to a record that the reader may see.
func articlesOf(ctx context.Context, q *dbgen.Queries, taskID uuid.UUID) ([]articleRefDTO, error) {
	rows, err := q.ListTaskArticles(ctx, taskID)
	out := make([]articleRefDTO, 0, len(rows))
	for _, a := range rows {
		out = append(out, articleRefDTO{ID: a.ID, Reference: articleRef(a.Number), Title: a.Title, Status: a.Status, Audience: a.Audience})
	}
	return out, err
}

// PUT /tickets/{id}/articles/{articleId} (update on the record's type): link an article to a record, such as the one
// that answers an incident. Its requester sees it too, when they may read the article. Answers the linked articles.
func (h *handlers) linkTicketArticle(w http.ResponseWriter, r *http.Request) {
	if out, ok := h.changeTicketArticle(w, r, true); ok {
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// DELETE /tickets/{id}/articles/{articleId} (update on the record's type): unlink an article. Answers the linked
// articles.
func (h *handlers) unlinkTicketArticle(w http.ResponseWriter, r *http.Request) {
	if out, ok := h.changeTicketArticle(w, r, false); ok {
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

func (h *handlers) changeTicketArticle(w http.ResponseWriter, r *http.Request, link bool) ([]articleRefDTO, bool) {
	articleID, ok := urlUUID(w, r, "articleId")
	if !ok {
		return nil, false
	}
	var out []articleRefDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		t, err := loadTicket(r, q)
		if err != nil {
			return err
		}
		if !p.Perms.CanRecord(authz.Update, t.Type) {
			return forbidden(fmt.Sprintf("linking articles to it needs %s", authz.Record(authz.Update, t.Type)))
		}
		if link {
			if _, err := q.GetArticle(r.Context(), articleID); errors.Is(err, pgx.ErrNoRows) {
				return invalid("there is no article with that id")
			} else if err != nil {
				return err
			}
			if err := q.LinkTaskArticle(r.Context(), dbgen.LinkTaskArticleParams{TaskID: t.ID, ArticleID: articleID}); err != nil {
				return err
			}
		} else if n, err := q.UnlinkTaskArticle(r.Context(), dbgen.UnlinkTaskArticleParams{TaskID: t.ID, ArticleID: articleID}); err != nil {
			return err
		} else if n == 0 {
			return notFound()
		}
		out, err = articlesOf(r.Context(), q, t.ID)
		return err
	}) {
		return nil, false
	}
	return out, true
}
