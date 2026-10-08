package cmdb

import (
	"testing"

	"github.com/servicelater/servicelater/backend/internal/apps"
)

func TestApplyFieldsChecksAgainstTheClass(t *testing.T) {
	c := &apps.Class{Label: "Server", All: []apps.FormField{
		{Name: "hostname", Type: apps.FieldText, Label: "Hostname", Required: true, MaxLength: 5},
		{Name: "cores", Type: apps.FieldNumber, Label: "Cores"},
	}}
	got, msg := ApplyFields(c, map[string]any{"hostname": "db1", "legacy": "kept"}, map[string]any{"cores": float64(8)})
	if msg != "" || got["cores"] != int64(8) || got["legacy"] != "kept" {
		t.Errorf("a number from JSON, an old value left alone: %v %q", got, msg)
	}
	for name, tc := range map[string]struct {
		changes map[string]any
		want    string
	}{
		"cleared required": {map[string]any{"hostname": nil}, "Hostname is required"},
		"too long":         {map[string]any{"hostname": "toolong"}, "Hostname is longer than 5 characters"},
		"unknown":          {map[string]any{"rack": "4"}, `Server items have no field "rack"`},
		"not a number":     {map[string]any{"cores": "eight"}, "Cores takes a whole number"},
	} {
		if _, msg := ApplyFields(c, map[string]any{"hostname": "db1"}, tc.changes); msg != tc.want {
			t.Errorf("%s: got %q, want %q", name, msg, tc.want)
		}
	}
}
