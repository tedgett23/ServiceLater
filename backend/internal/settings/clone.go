package settings

import (
	"context"
	"encoding/json"

	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// ClonePrefix names this instance's clone settings (Admin → Clone in).
const ClonePrefix = "clone."

// Clone is what this instance does about clones loaded into it (docs/backup-and-clone.md §2).
type Clone struct {
	// AllowIntoProduction lets a clone be loaded into an instance marked production. Off, it is refused, so a wrong
	// URL can never clone development over production.
	AllowIntoProduction bool `json:"allow_into_production"`
	// The safety switch: after a clone, until an admin lifts each, http.request steps fail, scheduled workflows do not
	// start, and webhooks answer 503. (Email, SMS, Teams and incoming mail are switched off in their own settings.)
	HoldHTTP      bool `json:"hold_http"`
	HoldSchedules bool `json:"hold_schedules"`
	HoldWebhooks  bool `json:"hold_webhooks"`
}

func (c *Clone) fields() map[string]*bool {
	return map[string]*bool{"allow_into_production": &c.AllowIntoProduction, "hold_http": &c.HoldHTTP,
		"hold_schedules": &c.HoldSchedules, "hold_webhooks": &c.HoldWebhooks}
}

// CloneFrom reads the clone settings; each is off unless stored.
func CloneFrom(stored map[string][]byte) Clone {
	var c Clone
	for name, ptr := range c.fields() {
		if raw, ok := stored[ClonePrefix+name]; ok {
			_ = json.Unmarshal(raw, ptr)
		}
	}
	return c
}

// Rows are the clone settings as they are stored.
func (c Clone) Rows() map[string][]byte {
	out := map[string][]byte{}
	for name, ptr := range c.fields() {
		out[ClonePrefix+name], _ = json.Marshal(*ptr)
	}
	return out
}

// Switched are the settings a clone turns off (each "<prefix>enabled"), what the safety switch is beyond the holds.
var Switched = []struct{ Name, Label, Page string }{
	{EmailPrefix + "enabled", "Outbound email", "/admin/email"},
	{SMSPrefix + "enabled", "SMS", "/admin/messaging"},
	{TeamsPrefix + "enabled", "Teams posts", "/admin/messaging"},
	{InboundPrefix + "enabled", "Incoming email", "/admin/incoming"},
}

// SettingsReader is what reads stored settings by prefix (dbgen.Queries.GetSystemSettings, as the system).
type SettingsReader interface {
	GetSystemSettings(ctx context.Context, prefix string) ([]dbgen.GetSystemSettingsRow, error)
}

// ReadClone reads the clone settings, as the system.
func ReadClone(ctx context.Context, q SettingsReader) (Clone, error) {
	rows, err := q.GetSystemSettings(ctx, ClonePrefix)
	stored := map[string][]byte{}
	for _, r := range rows {
		stored[r.Name] = r.Value
	}
	return CloneFrom(stored), err
}
