package records

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
)

// MayOrder reports whether a person may see and order a catalog item: it is open to everyone, or they hold one of
// its roles (as their base role, directly or through a group) or are in one of its groups.
func MayOrder(ctx context.Context, q *dbgen.Queries, person uuid.UUID, it *apps.CatalogItem) (bool, error) {
	if it.AvailableToAll() {
		return true, nil
	}
	groups := make([]string, len(it.AvailableTo.Groups))
	for i, g := range it.AvailableTo.Groups {
		groups[i] = strings.ToLower(g)
	}
	return q.InAudience(ctx, dbgen.InAudienceParams{UserID: person, Roles: append([]string{}, it.AvailableTo.Roles...), Groups: groups})
}

// itemTitle is a new request's summary: the item's title worked out from the answers, or its label.
func itemTitle(ctx context.Context, q *dbgen.Queries, a Actor, it *apps.CatalogItem, draft Ticket) (string, error) {
	v, err := it.TitleProgram()
	if err != nil {
		slog.Error("catalog item title does not compile", "app", it.App, "item", it.Name, "err", err)
		return "", Invalid(formBroken)
	}
	if v == nil {
		return it.Label, nil
	}
	if v.Expr == "" {
		s, _ := v.Literal.(string)
		return s, nil
	}
	p, err := v.Compiled()
	if err != nil || p == nil {
		return "", Invalid(formBroken)
	}
	u, err := q.GetUser(ctx, a.Person())
	if err != nil {
		return "", err
	}
	x, err := p.EvalValue(ctx, expr.Input{Record: draft.ExprRecord(), Now: time.Now(), Event: apps.EventCreate,
		User: &expr.User{ID: u.ID.String(), Name: u.Name, Email: u.Email, Role: u.Role}})
	if err != nil {
		slog.Error("catalog item title", "app", it.App, "item", it.Name, "err", err)
		return "", Invalid(formBroken)
	}
	s, _ := x.(string)
	if strings.TrimSpace(s) == "" {
		return it.Label, nil
	}
	if r := []rune(strings.TrimSpace(s)); len(r) > MaxTitle {
		return string(r[:MaxTitle]), nil
	}
	return strings.TrimSpace(s), nil
}

// startItemWorkflow starts the item's fulfilment workflow on the new request, for the person who ordered it. A
// workflow removed since the item was installed is logged and skipped: the request stands, and staff work it by hand.
func (s *Service) startItemWorkflow(ctx context.Context, q *dbgen.Queries, a Actor, cause ruleruns.Cause, it *apps.CatalogItem, t Ticket) error {
	if it.Workflow == "" {
		return nil
	}
	wf := s.Apps.Workflow(it.App, it.Workflow)
	if wf == nil {
		slog.Error("catalog item's workflow is not installed", "app", it.App, "item", it.Name, "workflow", it.Workflow)
		return nil
	}
	_, err := s.StartWorkflow(ctx, q, a, cause, t, wf)
	return err
}
