package records

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/rules"
)

// MaxMentions is how many people one comment may mention.
const MaxMentions = 20

// CommentInput is a comment a person adds to a record.
type CommentInput struct {
	Body     string
	Internal bool
	// Mentions are the people it @mentions. Each must be able to read it: see the record, and for an internal note
	// hold comments:internal.
	Mentions []uuid.UUID
}

// Comment adds a person's comment to a record, with the people it mentions, then runs the after rules on: [comment]
// in the same transaction (they tell people about it, and may refuse it). This is the one way a person's comment is
// written, from the app or from an email reply; comments that apps write are not comment events.
func (s *Service) Comment(ctx context.Context, q *dbgen.Queries, a Actor, cause ruleruns.Cause, id uuid.UUID, in CommentInput) (dbgen.TaskComment, error) {
	body := strings.TrimSpace(in.Body)
	if utf8.RuneCountInString(body) < 1 || len(body) > MaxComment {
		return dbgen.TaskComment{}, Invalid(fmt.Sprintf("comment must be 1 to %d characters", MaxComment))
	}
	if in.Internal && !a.Can(authz.CommentsInternal) {
		return dbgen.TaskComment{}, Forbidden("internal notes need the comments:internal permission")
	}
	t, err := Load(ctx, q, id)
	if err != nil {
		return dbgen.TaskComment{}, err
	}
	mentions, err := mentionable(ctx, q, t.ID, in)
	if err != nil {
		return dbgen.TaskComment{}, err
	}
	c, err := q.CreateComment(ctx, dbgen.CreateCommentParams{TaskID: t.ID, AuthorID: a.ID, Body: body, Internal: in.Internal})
	if err != nil {
		return c, err
	}
	if len(mentions) > 0 {
		if err := q.AddCommentMentions(ctx, dbgen.AddCommentMentionsParams{CommentID: c.ID, UserIds: mentions}); err != nil {
			return c, err
		}
	}
	list := s.Apps.Rules(t.Type, apps.TimingAfter, apps.EventComment)
	if len(list) == 0 {
		return c, nil
	}
	rin, err := s.ruleInput(ctx, q, a, apps.EventComment, &t, t)
	if err != nil {
		return c, err
	}
	rin.Comment = &expr.Comment{ID: c.ID.String(), Body: c.Body, Internal: c.Internal, AuthorID: a.ID.String(), Mentions: make([]string, len(mentions))}
	for i, m := range mentions {
		rin.Comment.Mentions[i] = m.String()
	}
	fx, err := rules.After(ctx, list, rin)
	if err != nil {
		return c, s.ruleError(ctx, err, apps.TimingAfter, apps.EventComment, &t.ID, t.Reference)
	}
	return c, s.applyAfter(ctx, q, a, cause, t, fx)
}

// mentionable checks the people a comment mentions, without repeats, and refuses one who could not read it.
func mentionable(ctx context.Context, q *dbgen.Queries, task uuid.UUID, in CommentInput) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	for _, id := range in.Mentions {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > MaxMentions {
		return nil, Invalid(fmt.Sprintf("a comment may mention at most %d people", MaxMentions))
	}
	ok, err := q.MentionablePeople(ctx, dbgen.MentionablePeopleParams{TaskID: task, UserIds: ids, Internal: in.Internal})
	if err != nil {
		return nil, err
	}
	allowed := map[uuid.UUID]bool{}
	for _, p := range ok {
		allowed[p.ID] = true
	}
	for _, id := range ids {
		if allowed[id] {
			continue
		}
		name := "Someone you mentioned"
		if u, err := q.GetUser(ctx, id); err == nil {
			name = u.Name
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if in.Internal {
			return nil, Invalid(name + " cannot read this internal note, so they cannot be mentioned in it")
		}
		return nil, Invalid(name + " cannot see this record, so they cannot be mentioned on it")
	}
	return ids, nil
}
