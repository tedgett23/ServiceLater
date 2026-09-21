package ruleruns

import (
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/apps"
)

func TestCauseBlocksLoops(t *testing.T) {
	mine := &apps.Rule{App: "house", Name: "a"}
	other := &apps.Rule{App: "other", Name: "b"}

	// A person's own save has no cause: anything may fire.
	if why := (Cause{}).Blocks(mine); why != "" {
		t.Errorf("a person's save blocked a rule: %s", why)
	}
	// Once a rule has run, what its save sets off may not be a rule of the same app, itself included...
	c := Cause{}.through(mine)
	if c.Depth != 1 || len(c.Chain) != 1 || c.Chain[0] != "house/a" {
		t.Fatalf("cause: %+v", c)
	}
	if why := c.Blocks(mine); !strings.Contains(why, "house/a") {
		t.Errorf("a rule re-triggered itself: %q", why)
	}
	if why := c.Blocks(&apps.Rule{App: "house", Name: "another"}); why == "" {
		t.Error("a rule of the same app was set off by its own app's change")
	}
	// ...but another app's rule may, so apps can still work together.
	if why := c.Blocks(other); why != "" {
		t.Errorf("a different app was blocked: %s", why)
	}
	// However many apps a chain crosses, it is cut at MaxDepth.
	deep := Cause{Depth: MaxDepth - 1}
	if why := deep.Blocks(other); why != "" {
		t.Errorf("depth %d blocked: %s", deep.Depth, why)
	}
	deep.Depth = MaxDepth
	if why := deep.Blocks(other); !strings.Contains(why, "deep") {
		t.Errorf("depth %d not blocked: %q", deep.Depth, why)
	}
	// through never shares the chain with the cause it grew from.
	a := Cause{Chain: make([]string, 1, 8)}
	b1, b2 := a.through(mine), a.through(other)
	if b1.Chain[1] == b2.Chain[1] {
		t.Error("two causes share a backing array")
	}
}
