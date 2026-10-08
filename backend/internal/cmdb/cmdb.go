// Package cmdb holds what the CMDB does beyond storing items (docs/apps-platform.md §8.2): checking an item's fields
// against its class, and working out what trouble with some items reaches.
//
// Items are core data (the cis table) and are read and written as the person under RLS like everything else; classes
// and relationship types are app definitions served by apps.Registry.
package cmdb

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// Limits of an impact walk: how many relationships away it looks, and how many items it reports.
const (
	MaxImpactDepth = 10
	MaxImpactItems = 1000
	maxStepRows    = 5000 // ImpactStep returns one more than this
)

// ApplyFields merges changes into an item's stored fields and checks the result against its class: each changed field
// must be one the class has, with a value of its type (normalised as stored), and every required field must be
// filled. A null or empty value clears a field. Stored values the class no longer allows are left alone unless
// changed, as a form does with a record's. The message says what is wrong, naming fields by their labels.
func ApplyFields(c *apps.Class, stored, changes map[string]any) (map[string]any, string) {
	out := make(map[string]any, len(stored)+len(changes))
	for k, v := range stored {
		out[k] = v
	}
	names := make([]string, 0, len(changes))
	for k := range changes {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, name := range names {
		v := changes[name]
		fd, ok := c.Field(name)
		if !ok {
			return nil, fmt.Sprintf("%s items have no field %q", c.Label, name)
		}
		if v == nil || v == "" {
			delete(out, name)
			continue
		}
		norm, msg := fd.Check(v)
		if msg != "" {
			return nil, fmt.Sprintf("%s %s", fd.Label, msg)
		}
		out[name] = norm
	}
	var missing []string
	for _, fd := range c.All {
		if v, ok := out[fd.Name]; fd.Required && (!ok || v == "") {
			missing = append(missing, fd.Label)
		}
	}
	if len(missing) > 0 {
		return nil, strings.Join(missing, ", ") + map[bool]string{true: " is required", false: " are required"}[len(missing) == 1]
	}
	return out, ""
}

// Affected is an item trouble reaches: how many relationships away it is and the item it is reached through.
type Affected struct {
	dbgen.GetCIsByIDRow
	Depth int
	Via   uuid.UUID
}

// Impact walks the relationships whose type carries impact backwards from the start items, breadth first, and lists
// every item reached with the shortest distance to it, nearest first. It reads as the caller (RLS), one query per
// step, never visits an item twice (so loops end), and stops at MaxImpactDepth steps or MaxImpactItems items, which
// truncated reports. The start items are not in the list.
func Impact(ctx context.Context, q *dbgen.Queries, start []uuid.UUID, types []string) (out []Affected, truncated bool, err error) {
	if len(start) == 0 || len(types) == 0 {
		return nil, false, nil
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range start {
		seen[id] = true
	}
	type reach struct {
		depth int
		via   uuid.UUID
	}
	reached := map[uuid.UUID]reach{}
	var order []uuid.UUID
	frontier := slices.Clone(start)
	for depth := 1; depth <= MaxImpactDepth && len(frontier) > 0; depth++ {
		steps, err := q.ImpactStep(ctx, dbgen.ImpactStepParams{Ids: frontier, Types: types})
		if err != nil {
			return nil, false, err
		}
		if len(steps) > maxStepRows { // ImpactStep's LIMIT: the rest of this step is not seen
			steps, truncated = steps[:maxStepRows], true
		}
		var next []uuid.UUID
		for _, s := range steps {
			if seen[s.FromCi] {
				continue
			}
			if len(order) >= MaxImpactItems {
				truncated = true
				break
			}
			seen[s.FromCi] = true
			reached[s.FromCi] = reach{depth, s.ToCi}
			order = append(order, s.FromCi)
			next = append(next, s.FromCi)
		}
		frontier = next
		if truncated {
			break
		}
		if depth == MaxImpactDepth && len(next) > 0 {
			more, err := q.ImpactStep(ctx, dbgen.ImpactStepParams{Ids: next, Types: types})
			if err != nil {
				return nil, false, err
			}
			truncated = slices.ContainsFunc(more, func(s dbgen.ImpactStepRow) bool { return !seen[s.FromCi] })
		}
	}
	if len(order) == 0 {
		return nil, truncated, nil
	}
	rows, err := q.GetCIsByID(ctx, order)
	if err != nil {
		return nil, false, err
	}
	for _, r := range rows {
		at := reached[r.ID]
		out = append(out, Affected{GetCIsByIDRow: r, Depth: at.depth, Via: at.via})
	}
	slices.SortFunc(out, func(a, b Affected) int {
		if a.Depth != b.Depth {
			return a.Depth - b.Depth
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out, truncated, nil
}
