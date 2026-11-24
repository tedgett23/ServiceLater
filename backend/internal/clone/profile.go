// Package clone makes clone packages (docs/backup-and-clone.md §2): a copy of this instance's records, people and
// definitions, filtered by a clone profile, for a UAT or development instance to load. Filtering happens here, on
// the source, so what a profile leaves out never leaves production. A package is like a snapshot (one stream: a
// plaintext header, then a tar archive compressed with zstd and encrypted with age), but encrypted with a passphrase
// the admin chose, since the target has another secrets key.
package clone

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// What a profile keeps of the records of the types it takes.
const (
	RecordsAll    = "all"
	RecordsOpen   = "open"   // open or waiting
	RecordsRecent = "recent" // changed in the last RecentDays days
)

// What comes with the kept records.
const (
	AttachmentsNone = "none"
	AttachmentsKept = "kept" // the files of the kept records, up to AttachmentMaxMB each

	HistoryNone   = "none"
	HistoryRecent = "recent" // the last HistoryDays days
	HistoryAll    = "all"

	PeopleAll        = "all"
	PeopleReferenced = "referenced" // only the people the kept records and the groups refer to
)

// Profile is what a clone takes. The zero value is not a profile: start from DefaultProfile.
type Profile struct {
	// RecordTypes are the types whose records come; empty is every type.
	RecordTypes []string `json:"record_types"`
	// Records is all, open or recent (RecentDays).
	Records    string `json:"records"`
	RecentDays int    `json:"recent_days"`
	// Exclude leaves out the records of a type an expression holds for (the rule environment: record, now).
	Exclude []Exclusion `json:"exclude"`
	// InternalNotes brings the kept records' internal notes; without it, only their public comments come.
	InternalNotes bool `json:"internal_notes"`
	// Attachments is none or kept; AttachmentMaxMB leaves out bigger files (0 is no limit).
	Attachments     string `json:"attachments"`
	AttachmentMaxMB int    `json:"attachment_max_mb"`
	// History is none, recent (HistoryDays) or all: the field-by-field history of the kept records and of items.
	History     string `json:"history"`
	HistoryDays int    `json:"history_days"`
	// Logs brings what was sent and run: notifications, email, SMS and Teams deliveries, incoming email, rule runs and
	// query runs.
	Logs bool `json:"logs"`
	// People is all or referenced.
	People string `json:"people"`
	// Passwords brings people's password hashes, so they sign in on the target as they do here.
	Passwords bool `json:"passwords"`
	Mask      Mask `json:"mask"`
}

// Mask is what a clone replaces or leaves out. Service accounts and apps are never masked: they are not people.
type Mask struct {
	// Names become "Person 12".
	Names bool `json:"names"`
	// Emails become user-12@masked.invalid (the target still matches people by their real address: the package holds
	// a salted hash of each).
	Emails bool `json:"emails"`
	// Phones leaves out people's phone numbers.
	Phones bool `json:"phones"`
	// Text replaces descriptions, comments, approval comments and incoming email subjects, and empties workflow
	// variables and step details.
	Text bool `json:"text"`
	// Fields leaves out the values of form fields marked mask: true.
	Fields bool `json:"fields"`
}

// Any reports whether anything is masked.
func (m Mask) Any() bool { return m.Names || m.Emails || m.Phones || m.Text || m.Fields }

// Exclusion leaves out the records of RecordType that When holds for.
type Exclusion struct {
	RecordType string `json:"record_type"`
	When       string `json:"when"`
}

// DefaultProfile takes everything, as it is: masking is off by default (the user's call).
var DefaultProfile = Profile{RecordTypes: []string{}, Exclude: []Exclusion{}, Records: RecordsAll, RecentDays: 90, InternalNotes: true, Attachments: AttachmentsKept,
	History: HistoryAll, HistoryDays: 90, People: PeopleAll, Passwords: true}

// ProfileFrom reads a stored profile over the default, so a field added later has its default.
func ProfileFrom(raw []byte) (Profile, error) {
	p := DefaultProfile
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, err
		}
	}
	p.lists()
	return p, nil
}

// lists makes the lists empty rather than null, as the API sends them.
func (p *Profile) lists() {
	if p.RecordTypes == nil {
		p.RecordTypes = []string{}
	}
	if p.Exclude == nil {
		p.Exclude = []Exclusion{}
	}
}

var profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// CheckName says what is wrong with a profile's name.
func CheckName(name string) error {
	if !profileName.MatchString(name) {
		return fmt.Errorf("name must be 1 to 40 lowercase letters, digits and dashes, starting with a letter or digit (such as uat)")
	}
	return nil
}

// Check says everything wrong with a profile. known reports whether a record type exists; compile checks an
// exclusion's expression for its type.
func (p *Profile) Check(known func(string) bool, compile func(recordType, when string) error) error {
	var bad []string
	add := func(format string, a ...any) { bad = append(bad, fmt.Sprintf(format, a...)) }
	p.lists()
	slices.Sort(p.RecordTypes)
	p.RecordTypes = slices.Compact(p.RecordTypes)
	for _, t := range p.RecordTypes {
		if !known(t) {
			add("record_types: there is no record type %q", t)
		}
	}
	switch p.Records {
	case RecordsAll, RecordsOpen:
	case RecordsRecent:
		if p.RecentDays < 1 || p.RecentDays > 3650 {
			add("recent_days must be between 1 and 3650")
		}
	default:
		add("records must be %s, %s or %s", RecordsAll, RecordsOpen, RecordsRecent)
	}
	for i := range p.Exclude {
		e := &p.Exclude[i]
		e.When = strings.TrimSpace(e.When)
		at := fmt.Sprintf("exclude %d", i+1)
		switch {
		case !known(e.RecordType):
			add("%s: there is no record type %q", at, e.RecordType)
		case len(p.RecordTypes) > 0 && !slices.Contains(p.RecordTypes, e.RecordType):
			add("%s: %s records are not cloned at all", at, e.RecordType)
		case e.When == "":
			add("%s: say which records to leave out", at)
		default:
			if err := compile(e.RecordType, e.When); err != nil {
				add("%s: %v", at, err)
			}
		}
	}
	if len(p.Exclude) > 20 {
		add("at most 20 exclusions")
	}
	if p.Attachments != AttachmentsNone && p.Attachments != AttachmentsKept {
		add("attachments must be %s or %s", AttachmentsNone, AttachmentsKept)
	}
	if p.AttachmentMaxMB < 0 || p.AttachmentMaxMB > 1<<20 {
		add("attachment_max_mb must be 0 (no limit) or more")
	}
	switch p.History {
	case HistoryNone, HistoryAll:
	case HistoryRecent:
		if p.HistoryDays < 1 || p.HistoryDays > 3650 {
			add("history_days must be between 1 and 3650")
		}
	default:
		add("history must be %s, %s or %s", HistoryNone, HistoryRecent, HistoryAll)
	}
	if p.People != PeopleAll && p.People != PeopleReferenced {
		add("people must be %s or %s", PeopleAll, PeopleReferenced)
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	return nil
}
