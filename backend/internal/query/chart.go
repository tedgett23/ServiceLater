package query

import (
	"fmt"
	"slices"
	"strings"
)

// Chart is how the workbench draws a result. Y are the
// numeric columns drawn, one series each, at most four (the chart palette's four colours, never cycled).
type Chart struct {
	// Type is line, area, scatter, bars, stacked, pie, doughnut or figure.
	Type string `json:"type"`
	// X is the axis or slice category; empty for a figure.
	X string `json:"x,omitempty"`
	// Y are the numeric columns drawn; area, scatter, pie, doughnut and figure take one measure.
	Y []string `json:"y"`
}

// ChartTypes are the charts the workbench draws.
var ChartTypes = []string{"line", "area", "scatter", "bars", "stacked", "pie", "doughnut", "figure"}

// MaxSeries is how many columns one chart draws: one colour each.
const MaxSeries = 4

// NumericTypes are the column types a chart can measure.
var NumericTypes = []string{"int2", "int4", "int8", "float4", "float8", "numeric"}

// Check says what is wrong with c for a result with these columns, or nil.
func (c Chart) Check(columns []Column) error {
	if !slices.Contains(ChartTypes, c.Type) {
		return fmt.Errorf("a chart must be one of %s, not %q", strings.Join(ChartTypes, ", "), c.Type)
	}
	types := make(map[string]string, len(columns))
	for _, col := range columns {
		types[col.Name] = col.Type
	}
	if c.Type == "figure" {
		if len(c.Y) != 1 {
			return fmt.Errorf("a figure shows one number: give one column")
		}
	} else {
		if c.X == "" {
			return fmt.Errorf("a %s chart needs a column along its axis", c.Type)
		}
		if _, ok := types[c.X]; !ok {
			return fmt.Errorf("the chart's axis, %s, is not a column of the view", c.X)
		}
		if c.Type == "scatter" && !slices.Contains(NumericTypes, types[c.X]) {
			return fmt.Errorf("a scatter chart needs a numeric X axis")
		}
		switch c.Type {
		case "area", "scatter", "pie", "doughnut":
			if len(c.Y) != 1 {
				return fmt.Errorf("a %s chart needs one measure", c.Type)
			}
		}
		if (c.Type == "scatter" || c.Type == "pie" || c.Type == "doughnut") && c.X == c.Y[0] {
			return fmt.Errorf("choose different columns for the axis and measure")
		}
		if len(c.Y) == 0 || len(c.Y) > MaxSeries {
			return fmt.Errorf("a chart draws 1 to %d columns", MaxSeries)
		}
	}
	seen := map[string]bool{}
	for _, y := range c.Y {
		t, ok := types[y]
		switch {
		case !ok:
			return fmt.Errorf("the chart draws %s, which is not a column of the view", y)
		case !slices.Contains(NumericTypes, t):
			return fmt.Errorf("the chart draws %s, which is %s: a chart measures numbers", y, t)
		case seen[y]:
			return fmt.Errorf("the chart draws %s twice", y)
		}
		seen[y] = true
	}
	return nil
}
