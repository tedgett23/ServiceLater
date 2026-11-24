package settings

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// BackupPrefix names the snapshot settings (Admin → Backups).
const BackupPrefix = "backup."

// Where snapshots are kept.
const (
	BackupFolder = "folder" // SL_SNAPSHOTS_DIR on the worker (shared with the api servers on a cluster)
	BackupS3     = "s3"     // the bucket of Admin → Storage, under <prefix>snapshots/
)

// Backup is when snapshots are taken, where they go and how many are kept (docs/backup-and-clone.md §1).
type Backup struct {
	// Enabled takes snapshots on the schedule; without it they are only taken by hand.
	Enabled bool `json:"enabled"`
	// Cron is when (minute hour day month weekday), in Timezone.
	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`
	// Destination is folder or s3.
	Destination string `json:"destination"`
	// Attachments includes the attachment files kept in a folder (S3 attachments stay in their bucket).
	Attachments bool `json:"attachments"`
	// Kept: the newest KeepLast, plus the newest of each of the last KeepDays days and KeepWeeks weeks. Snapshots
	// taken by hand or uploaded are kept until someone deletes them.
	KeepLast  int `json:"keep_last"`
	KeepDays  int `json:"keep_days"`
	KeepWeeks int `json:"keep_weeks"`
}

// DefaultBackup takes nothing until an admin turns it on: a snapshot needs a secrets key and somewhere to go.
var DefaultBackup = Backup{Cron: "0 2 * * *", Timezone: "UTC", Destination: BackupFolder, KeepLast: 7, KeepDays: 14, KeepWeeks: 8}

var backupCron = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

func (b *Backup) fields() map[string]any {
	return map[string]any{"enabled": &b.Enabled, "cron": &b.Cron, "timezone": &b.Timezone, "destination": &b.Destination,
		"attachments": &b.Attachments, "keep_last": &b.KeepLast, "keep_days": &b.KeepDays, "keep_weeks": &b.KeepWeeks}
}

// BackupFrom reads the backup settings, using the default for any that is missing or unreadable.
func BackupFrom(stored map[string][]byte) Backup {
	b := DefaultBackup
	for name, ptr := range b.fields() {
		if raw, ok := stored[BackupPrefix+name]; ok {
			_ = json.Unmarshal(raw, ptr)
		}
	}
	return b
}

// Rows are the backup settings as they are stored.
func (b Backup) Rows() map[string][]byte {
	out := map[string][]byte{}
	for name, ptr := range b.fields() {
		out[BackupPrefix+name], _ = json.Marshal(ptr)
	}
	return out
}

// Schedule is the backup cron line in its time zone.
func (b Backup) Schedule() (cron.Schedule, *time.Location, error) {
	loc, err := time.LoadLocation(b.Timezone)
	if err != nil {
		return nil, nil, fmt.Errorf("timezone %q is not a time zone (such as Europe/London)", b.Timezone)
	}
	s, err := backupCron.Parse(b.Cron)
	if err != nil {
		return nil, nil, fmt.Errorf("cron %q: %w (five fields: minute hour day month weekday, such as \"0 2 * * *\")", b.Cron, err)
	}
	return s, loc, nil
}

// Check says what is wrong.
func (b *Backup) Check() error {
	b.Cron, b.Timezone = strings.TrimSpace(b.Cron), strings.TrimSpace(b.Timezone)
	var bad []string
	if _, _, err := b.Schedule(); err != nil {
		bad = append(bad, err.Error())
	}
	if b.Destination != BackupFolder && b.Destination != BackupS3 {
		bad = append(bad, fmt.Sprintf("destination must be %s or %s", BackupFolder, BackupS3))
	}
	if b.KeepLast < 1 || b.KeepLast > 1000 {
		bad = append(bad, "keep_last must be between 1 and 1000")
	}
	if b.KeepDays < 0 || b.KeepDays > 3650 || b.KeepWeeks < 0 || b.KeepWeeks > 520 {
		bad = append(bad, "keep_days must be 0 to 3650 and keep_weeks 0 to 520")
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	return nil
}

// InstancePrefix names what this instance is (Admin → Instance).
const InstancePrefix = "instance."

// Environments an instance can be.
const (
	EnvProduction  = "production"
	EnvUAT         = "uat"
	EnvDevelopment = "development"
	EnvOther       = "other"
)

// Instance is this instance's name and environment. Instances that are not production show a banner on every page,
// so nobody mistakes one for another.
type Instance struct {
	Name        string `json:"name"`
	Environment string `json:"environment"`
}

// DefaultInstance is a production instance with no name.
var DefaultInstance = Instance{Environment: EnvProduction}

// InstanceFrom reads the instance settings.
func InstanceFrom(stored map[string][]byte) Instance {
	in := DefaultInstance
	if raw, ok := stored[InstancePrefix+"name"]; ok {
		_ = json.Unmarshal(raw, &in.Name)
	}
	if raw, ok := stored[InstancePrefix+"environment"]; ok {
		_ = json.Unmarshal(raw, &in.Environment)
	}
	return in
}

// Rows are the instance settings as they are stored.
func (in Instance) Rows() map[string][]byte {
	name, _ := json.Marshal(in.Name)
	env, _ := json.Marshal(in.Environment)
	return map[string][]byte{InstancePrefix + "name": name, InstancePrefix + "environment": env}
}

// Check says what is wrong.
func (in *Instance) Check() error {
	in.Name = strings.TrimSpace(in.Name)
	if len(in.Name) > 60 {
		return fmt.Errorf("name is longer than 60 characters")
	}
	switch in.Environment {
	case EnvProduction, EnvUAT, EnvDevelopment, EnvOther:
		return nil
	}
	return fmt.Errorf("environment must be %s, %s, %s or %s", EnvProduction, EnvUAT, EnvDevelopment, EnvOther)
}
