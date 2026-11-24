package query_test

import (
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/query"
)

func TestAChartDrawsColumnsTheViewHas(t *testing.T) {
	cols := []query.Column{{Name: "team", Type: "text"}, {Name: "open", Type: "int8"}, {Name: "breached", Type: "int8"},
		{Name: "week", Type: "timestamptz"}, {Name: "avg_hours", Type: "numeric"}}
	for _, c := range []query.Chart{
		{Type: "bars", X: "team", Y: []string{"open", "breached"}},
		{Type: "stacked", X: "team", Y: []string{"open"}},
		{Type: "line", X: "week", Y: []string{"avg_hours"}},
		{Type: "area", X: "week", Y: []string{"avg_hours"}},
		{Type: "scatter", X: "open", Y: []string{"avg_hours"}},
		{Type: "pie", X: "team", Y: []string{"open"}},
		{Type: "doughnut", X: "team", Y: []string{"open"}},
		{Type: "figure", Y: []string{"open"}},
	} {
		if err := c.Check(cols); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	for want, c := range map[string]query.Chart{
		"not \"map\"":                 {Type: "map", X: "team", Y: []string{"open"}},
		"numeric X axis":              {Type: "scatter", X: "team", Y: []string{"open"}},
		"different columns":           {Type: "pie", X: "open", Y: []string{"open"}},
		"one measure":                 {Type: "doughnut", X: "team", Y: []string{"open", "breached"}},
		"needs a column along":        {Type: "line", Y: []string{"open"}},
		"nosuch, is not a column":     {Type: "bars", X: "nosuch", Y: []string{"open"}},
		"which is text":               {Type: "bars", X: "week", Y: []string{"team"}},
		"1 to 4 columns":              {Type: "bars", X: "team", Y: []string{"open", "breached", "avg_hours", "open", "breached"}},
		"twice":                       {Type: "bars", X: "team", Y: []string{"open", "open"}},
		"one number":                  {Type: "figure", Y: []string{"open", "breached"}},
		"gone, which is not a column": {Type: "figure", Y: []string{"gone"}},
	} {
		if err := c.Check(cols); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: got %v, want %q", c, err, want)
		}
	}
}
