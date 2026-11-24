package settings

import (
	"encoding/json"
	"fmt"
	"strings"
)

// LimitsPrefix names the rate limit settings (Admin → Rate limits).
const LimitsPrefix = "limits."

// Limits are how much one caller may do: the API, signing in, webhooks, SQL queries, SMS codes and live connections.
// Every server reads them from the database, so a change reaches all of them (within a second for the API limit,
// which is kept in memory; the others are read when they are used). 0 turns off the limits that may be off.
type Limits struct {
	// APIPeoplePerMinute is how many API requests a signed-in person may make in a minute (0: no limit).
	APIPeoplePerMinute int `json:"api_people_per_minute"`
	// APIKeysPerMinute is how many an API key may make in a minute, unless the key has its own limit (0: no limit).
	APIKeysPerMinute int `json:"api_keys_per_minute"`
	// SignInAttempts is how many times one address may try to sign in from one place in SignInWindowMinutes.
	SignInAttempts      int `json:"sign_in_attempts"`
	SignInWindowMinutes int `json:"sign_in_window_minutes"`
	// WebhookPerMinute is how many calls one webhook takes in a minute, refused ones included (0: no limit).
	WebhookPerMinute int `json:"webhook_per_minute"`
	// QueriesPerHour and QueriesAtOnce are how many SQL queries one person may start in an hour and have running at
	// once (on one server).
	QueriesPerHour int `json:"queries_per_hour"`
	QueriesAtOnce  int `json:"queries_at_once"`
	// SMSCodesPerHour is how many confirmation codes one person may have texted in an hour.
	SMSCodesPerHour int `json:"sms_codes_per_hour"`
	// LiveConnections is how many live (WebSocket) connections one person may hold at once, on one server: their open
	// tabs and devices.
	LiveConnections int `json:"live_connections"`
}

// DefaultLimits are the limits until an admin changes them.
var DefaultLimits = Limits{APIPeoplePerMinute: 1200, APIKeysPerMinute: 1200, SignInAttempts: 10, SignInWindowMinutes: 15,
	WebhookPerMinute: 300, QueriesPerHour: 300, QueriesAtOnce: 2, SMSCodesPerHour: 5, LiveConnections: 10}

// limitRange is what a limit may be set to; off says 0 is allowed (no limit).
type limitRange struct {
	min, max int
	off      bool
	label    string
}

func (l *Limits) fields() []struct {
	name string
	v    *int
	r    limitRange
} {
	return []struct {
		name string
		v    *int
		r    limitRange
	}{
		{"api_people_per_minute", &l.APIPeoplePerMinute, limitRange{10, 1_000_000, true, "API requests a person may make a minute"}},
		{"api_keys_per_minute", &l.APIKeysPerMinute, limitRange{10, 1_000_000, true, "API requests a key may make a minute"}},
		{"sign_in_attempts", &l.SignInAttempts, limitRange{3, 1000, false, "sign-in attempts"}},
		{"sign_in_window_minutes", &l.SignInWindowMinutes, limitRange{1, 1440, false, "the sign-in window in minutes"}},
		{"webhook_per_minute", &l.WebhookPerMinute, limitRange{1, 1_000_000, true, "calls a webhook takes a minute"}},
		{"queries_per_hour", &l.QueriesPerHour, limitRange{1, 100_000, false, "SQL queries a person may run an hour"}},
		{"queries_at_once", &l.QueriesAtOnce, limitRange{1, 20, false, "SQL queries a person may run at once"}},
		{"sms_codes_per_hour", &l.SMSCodesPerHour, limitRange{1, 50, false, "SMS codes a person may be sent an hour"}},
		{"live_connections", &l.LiveConnections, limitRange{1, 100, false, "live connections a person may hold"}},
	}
}

// LimitsFrom reads the limits from stored settings, using the default for any that is missing or unreadable.
func LimitsFrom(stored map[string][]byte) Limits {
	l := DefaultLimits
	for _, f := range l.fields() {
		if raw, ok := stored[LimitsPrefix+f.name]; ok {
			var v int
			if json.Unmarshal(raw, &v) == nil && f.r.allows(v) {
				*f.v = v
			}
		}
	}
	return l
}

// Rows are the limits as they are stored.
func (l Limits) Rows() map[string][]byte {
	out := map[string][]byte{}
	for _, f := range l.fields() {
		out[LimitsPrefix+f.name], _ = json.Marshal(*f.v)
	}
	return out
}

func (r limitRange) allows(v int) bool { return (r.off && v == 0) || (v >= r.min && v <= r.max) }

// Check says every limit that is out of range.
func (l *Limits) Check() error {
	var bad []string
	for _, f := range l.fields() {
		if f.r.allows(*f.v) {
			continue
		}
		msg := fmt.Sprintf("%s: %s must be between %d and %d", f.name, f.r.label, f.r.min, f.r.max)
		if f.r.off {
			msg += ", or 0 for no limit"
		}
		bad = append(bad, msg)
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	return nil
}
