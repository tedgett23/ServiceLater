// Package apps is the app platform: it reads and validates app bundles, installs their definitions into
// Postgres, and serves the current definitions to the rest of the system. See docs/apps-platform.md.
package apps

import (
	"fmt"
	"strconv"
	"strings"
)

// PlatformVersion is the version of this ServiceLater build, checked against an app's ServiceLater_min_version.
const PlatformVersion = "0.1.0"

// Version is a plain major.minor.patch version.
type Version struct{ Major, Minor, Patch int }

func ParseVersion(s string) (Version, error) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("%q is not a version like 1.2.3", s)
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || p != strconv.Itoa(v) {
			return Version{}, fmt.Errorf("%q is not a version like 1.2.3", s)
		}
		n[i] = v
	}
	return Version{n[0], n[1], n[2]}, nil
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Compare returns -1, 0 or 1.
func (v Version) Compare(o Version) int {
	for _, d := range [3]int{v.Major - o.Major, v.Minor - o.Minor, v.Patch - o.Patch} {
		if d < 0 {
			return -1
		}
		if d > 0 {
			return 1
		}
	}
	return 0
}

// Constraint is a comma-separated list of comparisons that must all hold, e.g. ">=1.0.0, <2.0.0".
// A bare version means exactly that version.
type Constraint struct{ clauses []clause }

type clause struct {
	op string
	v  Version
}

func ParseConstraint(s string) (Constraint, error) {
	var c Constraint
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return Constraint{}, fmt.Errorf("empty version constraint %q", s)
		}
		op := "="
		for _, candidate := range []string{">=", "<=", ">", "<", "="} {
			if strings.HasPrefix(part, candidate) {
				op, part = candidate, strings.TrimSpace(strings.TrimPrefix(part, candidate))
				break
			}
		}
		v, err := ParseVersion(part)
		if err != nil {
			return Constraint{}, fmt.Errorf("version constraint %q: %w", s, err)
		}
		c.clauses = append(c.clauses, clause{op, v})
	}
	return c, nil
}

func (c Constraint) Satisfied(v Version) bool {
	for _, cl := range c.clauses {
		cmp := v.Compare(cl.v)
		ok := map[string]bool{
			">=": cmp >= 0, "<=": cmp <= 0, ">": cmp > 0, "<": cmp < 0, "=": cmp == 0,
		}[cl.op]
		if !ok {
			return false
		}
	}
	return true
}
