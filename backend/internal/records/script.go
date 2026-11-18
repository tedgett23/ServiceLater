package records

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/rules"
	"github.com/servicelater/servicelater/backend/internal/script"
)

// maxChildren is the most records children() hands a script at once.
const maxChildren = 500

var referenceRE = regexp.MustCompile(`^([A-Za-z]{2,5})([0-9]+)$`)

// ScriptReader is how a script reads records other than its own: through q, so as whoever the transaction runs as,
// under their RLS. A record they cannot see is not there.
func ScriptReader(q *dbgen.Queries) script.Reader { return scriptReader{q} }

type scriptReader struct{ q *dbgen.Queries }

func (r scriptReader) Children(ctx context.Context, parentID string, open bool) ([]*expr.Record, error) {
	id, err := uuid.Parse(parentID)
	if err != nil {
		return nil, nil
	}
	rows, err := r.q.ListChildTasks(ctx, uuid.NullUUID{UUID: id, Valid: true})
	if err != nil {
		return nil, err
	}
	var out []*expr.Record
	for _, row := range rows {
		if open && row.Task.StateCategory != "open" && row.Task.StateCategory != "waiting" {
			continue
		}
		if len(out) == maxChildren {
			return nil, fmt.Errorf("more than %d records are under it", maxChildren)
		}
		t, err := Load(ctx, r.q, row.Task.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, t.ExprRecord())
	}
	return out, nil
}

func (r scriptReader) Get(ctx context.Context, ref string) (*expr.Record, error) {
	id, err := r.resolve(ctx, ref)
	if err != nil || id == uuid.Nil {
		return nil, err
	}
	t, err := Load(ctx, r.q, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t.ExprRecord(), nil
}

// resolve finds a record's id from its id or reference; uuid.Nil when there is none to see.
func (r scriptReader) resolve(ctx context.Context, ref string) (uuid.UUID, error) {
	if id, err := uuid.Parse(ref); err == nil {
		return id, nil
	}
	m := referenceRE.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return uuid.Nil, nil
	}
	n, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil || n < 1 {
		return uuid.Nil, nil
	}
	t, err := r.q.GetTaskByReference(ctx, dbgen.GetTaskByReferenceParams{Prefix: strings.ToUpper(m[1]), Number: n})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil
	}
	return t.ID, err
}

// ScriptError is a change a script asked for that the save path refused: which record, and why.
type ScriptError struct {
	Reference string
	Err       error
}

func (e *ScriptError) Error() string { return fmt.Sprintf("%s: %v", e.Reference, e.Err) }
func (e *ScriptError) Unwrap() error { return e.Err }

// ApplyScript makes the changes a script asked for, in order, through the save path as a: for each update, the
// fields as one edit, then the move to a status (guards and all), then its comment; then the comments. A refused
// change is a *ScriptError naming the record, and the caller's transaction undoes the rest.
func (s *Service) ApplyScript(ctx context.Context, q *dbgen.Queries, a Actor, cause ruleruns.Cause, res *script.Result) error {
	for _, u := range res.Updates {
		id, err := uuid.Parse(u.Record)
		if err != nil {
			return fmt.Errorf("script: %q is not a record id", u.Record)
		}
		t, err := Load(ctx, q, id)
		if err != nil {
			return err
		}
		fail := func(err error) error { return &ScriptError{Reference: t.Reference, Err: err} }
		if len(u.Fields) > 0 {
			rec := t.ExprRecord()
			for name, v := range u.Fields {
				if err := rules.Assign(rec, name, v); err != nil {
					return fail(err)
				}
			}
			want, err := FromRecord(ctx, q, t, rec)
			if err != nil {
				return fail(err)
			}
			if p := PatchBetween(t, want); !p.Empty() {
				saved, err := s.Update(ctx, q, a, cause, t.ID, p)
				if err != nil {
					return fail(err)
				}
				t = saved
			}
		}
		if u.Status != "" && u.Status != t.Status {
			if _, err := s.Transition(ctx, q, a, cause, t.ID, u.Status); err != nil {
				return fail(err)
			}
		}
		if u.Comment != "" {
			if err := scriptComment(ctx, q, a, t.ID, u.Comment, u.Internal); err != nil {
				return fail(err)
			}
		}
	}
	for _, c := range res.Comments {
		id, err := uuid.Parse(c.Record)
		if err != nil {
			return fmt.Errorf("script: %q is not a record id", c.Record)
		}
		if err := scriptComment(ctx, q, a, id, c.Body, c.Internal); err != nil {
			return &ScriptError{Reference: c.Record, Err: err}
		}
	}
	return nil
}

func scriptComment(ctx context.Context, q *dbgen.Queries, a Actor, task uuid.UUID, body string, internal bool) error {
	body = strings.TrimSpace(body)
	if body == "" || len(body) > MaxComment {
		return fmt.Errorf("a comment must be 1 to %d characters", MaxComment)
	}
	_, err := q.CreateComment(ctx, dbgen.CreateCommentParams{TaskID: task, AuthorID: a.ID, Body: body, Internal: internal})
	return err
}
