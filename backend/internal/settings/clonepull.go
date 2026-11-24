package settings

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// ClonePullPrefix names where this instance pulls clones from (Admin → Clone in → Source).
const ClonePullPrefix = "clone.pull."

// ClonePullKey is the secret holding the source's pull key.
const ClonePullKey = "clone.pull.key" //nolint:gosec // the name of a secret, not a secret

// ClonePull is the source a UAT or development instance pulls clone packages from, and when (docs/backup-and-clone.md
// §2, "Scheduled pull"). The key is a secret of its own (ClonePullKey), never a setting.
type ClonePull struct {
	// URL is the source's address (https://servicelater.example.com); http only to this machine.
	URL string `json:"url"`
	// Enabled pulls and applies a package on the schedule. Pulling by hand needs only the URL and the key.
	Enabled bool `json:"enabled"`
	// Cron is when (minute hour day month weekday), in Timezone.
	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`
	// KeepPeople and KeepStudio are what a scheduled pull keeps of this instance's own, as when applying by hand.
	KeepPeople bool `json:"keep_people"`
	KeepStudio bool `json:"keep_studio"`
	// ConfirmedBy is who turned the schedule on, typing the instance's name: each scheduled clone is applied as theirs
	// (so they are always kept), as if they had applied it.
	ConfirmedBy     string `json:"confirmed_by"`
	ConfirmedByName string `json:"confirmed_by_name"`
}

// DefaultClonePull pulls nothing until an admin sets a source and turns it on; then every Sunday at 02:00.
var DefaultClonePull = ClonePull{Cron: "0 2 * * 0", Timezone: "UTC", KeepPeople: true, KeepStudio: true}

func (c *ClonePull) fields() map[string]any {
	return map[string]any{"url": &c.URL, "enabled": &c.Enabled, "cron": &c.Cron, "timezone": &c.Timezone,
		"keep_people": &c.KeepPeople, "keep_studio": &c.KeepStudio, "confirmed_by": &c.ConfirmedBy,
		"confirmed_by_name": &c.ConfirmedByName}
}

// ClonePullFrom reads the pull settings, using the default for any that is missing or unreadable.
func ClonePullFrom(stored map[string][]byte) ClonePull {
	c := DefaultClonePull
	for name, ptr := range c.fields() {
		if raw, ok := stored[ClonePullPrefix+name]; ok {
			_ = json.Unmarshal(raw, ptr)
		}
	}
	return c
}

// Rows are the pull settings as they are stored.
func (c ClonePull) Rows() map[string][]byte {
	out := map[string][]byte{}
	for name, ptr := range c.fields() {
		out[ClonePullPrefix+name], _ = json.Marshal(ptr)
	}
	return out
}

// Schedule is the pull's cron line in its time zone.
func (c ClonePull) Schedule() (cron.Schedule, *time.Location, error) {
	return Backup{Cron: c.Cron, Timezone: c.Timezone}.Schedule()
}

// Check says what is wrong, tidying what it can.
func (c *ClonePull) Check() error {
	c.URL = strings.TrimRight(strings.TrimSpace(c.URL), "/")
	c.Cron, c.Timezone = strings.TrimSpace(c.Cron), strings.TrimSpace(c.Timezone)
	var bad []string
	if c.URL != "" {
		if err := CheckSourceURL(c.URL); err != nil {
			bad = append(bad, err.Error())
		}
	}
	if _, _, err := c.Schedule(); err != nil {
		bad = append(bad, err.Error())
	}
	if c.Enabled && c.URL == "" {
		bad = append(bad, "the schedule needs the source's address")
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	return nil
}

// CheckSourceURL says whether a clone source's address may be used: https, or http to this machine only, since the
// pull key and the package's passphrase travel with every request.
func CheckSourceURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("the source's address must be like https://servicelater.example.com")
	}
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		return fmt.Errorf("the source's address must use https (http only to this machine): the pull key travels with every request")
	}
	return nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
