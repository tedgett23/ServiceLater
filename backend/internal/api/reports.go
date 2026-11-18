package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/cmdb"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/slaclock"
)

// The SLA report (reports:read): how records met their SLAs over a period, what is at risk now and what breached
// lately. Every figure is worked out as the reader, so records they cannot see (a restricted group's) do not count,
// and working time is each clock's own: the business hours it counted by.

const (
	// atRiskShare is the share of its target a running clock has used when it counts as at risk.
	atRiskShare = 0.75
	// maxOpenClocks bounds the running clocks one report reads; past it the report says it is partial.
	maxOpenClocks   = 5000
	maxReportRows   = 10
	maxReportExport = 100000
)

// reportPeriods are the periods the report offers: how far back, and the bucket the trend is counted in.
var reportPeriods = map[string]struct {
	back   func(time.Time) time.Time
	bucket string
}{
	"7d":  {func(t time.Time) time.Time { return t.AddDate(0, 0, -7) }, "day"},
	"30d": {func(t time.Time) time.Time { return t.AddDate(0, 0, -30) }, "week"},
	"90d": {func(t time.Time) time.Time { return t.AddDate(0, 0, -90) }, "week"},
	"12m": {func(t time.Time) time.Time { return t.AddDate(-1, 0, 0) }, "month"},
}

// reportFilter is what a report is narrowed to, as the queries take it.
type reportFilter struct {
	period   string
	from, to time.Time
	bucket   string
	typ      pgtype.Text
	app, sla pgtype.Text
	group    uuid.NullUUID
	noGroup  bool
	priority pgtype.Int4
}

// readReportFilter reads ?period=7d|30d|90d|12m&type=&sla=app/name&group=<id>|none&priority=1..4.
func readReportFilter(v url.Values, now time.Time) (reportFilter, error) {
	f := reportFilter{period: v.Get("period"), to: now}
	if f.period == "" {
		f.period = "30d"
	}
	p, ok := reportPeriods[f.period]
	if !ok {
		return f, invalid("period must be 7d, 30d, 90d or 12m")
	}
	f.from, f.bucket = p.back(now), p.bucket
	if t := v.Get("type"); t != "" {
		f.typ = pgtype.Text{String: t, Valid: true}
	}
	if s := v.Get("sla"); s != "" {
		app, name, ok := strings.Cut(s, "/")
		if !ok || app == "" || name == "" {
			return f, invalid("sla must be app/name")
		}
		f.app, f.sla = pgtype.Text{String: app, Valid: true}, pgtype.Text{String: name, Valid: true}
	}
	switch g := v.Get("group"); g {
	case "":
	case "none":
		f.noGroup = true
	default:
		id, err := uuid.Parse(g)
		if err != nil {
			return f, invalid("group must be a group's id or none")
		}
		f.group = uuid.NullUUID{UUID: id, Valid: true}
	}
	if s := v.Get("priority"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 4 {
			return f, invalid("priority must be 1 to 4")
		}
		f.priority = pgtype.Int4{Int32: int32(n), Valid: true} //nolint:gosec // 1 to 4
	}
	return f, nil
}

func (f reportFilter) ended(from, to time.Time) dbgen.SLAReportEndedParams {
	return dbgen.SLAReportEndedParams{Bucket: f.bucket, From: reportTS(from), To: reportTS(to), Type: f.typ, App: f.app, Sla: f.sla,
		GroupID: f.group, NoGroup: f.noGroup, Priority: f.priority}
}

func reportTS(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// share is part of whole as a percentage to one decimal, or nil when there is nothing to share.
func share(part, whole int64) *float64 {
	if whole == 0 {
		return nil
	}
	v := float64(int64(float64(part)*1000/float64(whole)+0.5)) / 10
	return &v
}

// slaReportDTO is the SLA report for a period.
type slaReportDTO struct {
	Period string    `json:"period"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	// Bucket is what each point of the trend counts: day, week or month.
	Bucket  string           `json:"bucket"`
	Summary slaReportSummary `json:"summary"`
	SLAs    []slaReportSLA   `json:"slas"`
	Trend   []slaReportPoint `json:"trend"`
	Groups  []slaReportBreak `json:"groups"`
	Prios   []slaReportBreak `json:"priorities"`
	Types   []slaReportBreak `json:"types"`
	AtRisk  []slaReportClock `json:"at_risk"`
	Breach  []slaReportClock `json:"breaches"`
	Options slaReportOptions `json:"options"`
	Partial bool             `json:"partial"`
	// Series are the trend's lines (one per SLA), in the order each point's values list them.
	Series []slaReportSeries `json:"series"`
}

// slaReportSummary is the headline: clocks that ended in the period and how many met their target, the same for the
// period before (to compare), and what is running and at risk now.
type slaReportSummary struct {
	Ended    int64    `json:"ended"`
	Met      int64    `json:"met"`
	Breached int64    `json:"breached"`
	MetPct   *float64 `json:"met_pct"`
	// PrevMetPct is the share met in the period of the same length before this one.
	PrevMetPct *float64 `json:"prev_met_pct"`
	Running    int      `json:"running"`
	AtRisk     int      `json:"at_risk"`
}

// slaReportSLA is one SLA's figures for the period: MedianMS is the median working time its ended clocks used, and
// MedianTargetMS their median target.
type slaReportSLA struct {
	Key            string   `json:"key"`
	Label          string   `json:"label"`
	Ended          int64    `json:"ended"`
	Met            int64    `json:"met"`
	MetPct         *float64 `json:"met_pct"`
	MedianMS       *int64   `json:"median_ms"`
	MedianTargetMS *int64   `json:"median_target_ms"`
}

// slaReportSeries is a line of the trend: an SLA, in the order the trend's points list it.
type slaReportSeries struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// slaReportPoint is one bucket of the trend: for each series (in Series order), how many clocks ended and met.
type slaReportPoint struct {
	Start  time.Time          `json:"start"`
	Values []slaReportCounted `json:"values"`
}

type slaReportCounted struct {
	Ended  int64    `json:"ended"`
	Met    int64    `json:"met"`
	MetPct *float64 `json:"met_pct"`
}

// slaReportBreak is one row of a breakdown (an assignment group, a priority, a type): clocks that ended in the period,
// met or breached, and those still running or paused now.
type slaReportBreak struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Ended    int64    `json:"ended"`
	Met      int64    `json:"met"`
	Breached int64    `json:"breached"`
	Running  int64    `json:"running"`
	MetPct   *float64 `json:"met_pct"`
}

// slaReportClock is a clock in a list: at risk now, or breached lately.
type slaReportClock struct {
	Reference string `json:"reference"`
	Title     string `json:"title"`
	Link      string `json:"link"`
	SLA       string `json:"sla"`
	Group     string `json:"group"`
	// UsedMS of TargetMS is the working time used; UsedPct the same as a whole percentage.
	UsedMS   int64 `json:"used_ms"`
	TargetMS int64 `json:"target_ms"`
	UsedPct  int   `json:"used_pct"`
	// DueAt is when a running clock's target is used up; OverMS how far past it a breached clock went.
	DueAt      *time.Time `json:"due_at,omitempty"`
	BreachedAt *time.Time `json:"breached_at,omitempty"`
	OverMS     int64      `json:"over_ms"`
	// Calendar is the label of the hours it counts by, "Every hour" when every hour counts.
	Calendar string `json:"calendar"`
}

// slaReportOptions are the choices the filters offer.
type slaReportOptions struct {
	SLAs       []slaReportSeries `json:"slas"`
	Types      []slaReportSeries `json:"types"`
	Priorities []slaReportSeries `json:"priorities"`
}

const noGroupLabel = "No group"

func groupKey(id uuid.NullUUID) string {
	if !id.Valid {
		return "none"
	}
	return id.UUID.String()
}

func groupLabel(name string) string {
	if name == "" {
		return noGroupLabel
	}
	return name
}

// priorityLabels are the priorities as people call them.
func priorityLabels() map[int16]string {
	out := map[int16]string{}
	for _, f := range apps.CoreFields {
		if f.Name != "priority" {
			continue
		}
		for _, o := range f.Options {
			if n, err := strconv.Atoi(o.Value); err == nil {
				out[int16(n)] = o.Label //nolint:gosec // 1 to 4
			}
		}
	}
	return out
}

// calendarLabels names the hours clocks count by, for the reader.
func calendarLabels(ctx context.Context, q *dbgen.Queries) (func(string) string, error) {
	cals, err := q.ListBusinessCalendars(ctx)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	for _, c := range cals {
		labels[c.BusinessCalendar.Name] = c.BusinessCalendar.Label
	}
	return func(name string) string {
		switch {
		case name == "":
			return "Every hour"
		case labels[name] != "":
			return labels[name]
		}
		return name
	}, nil
}

// GET /reports/slas (reports:read) with ?period=7d|30d|90d|12m (default 30d), type, sla (app/name), group (an id or
// none) and priority: how clocks that ended in the period met their SLAs (overall, against the period before, per SLA,
// over time, by assignment group, priority and type), the running clocks most at risk now and the latest breaches.
// Working time is each clock's own business hours. Only records the reader may see count.
func (h *handlers) slaReport(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	f, err := readReportFilter(r.URL.Query(), now)
	if err != nil {
		writeDBError(w, err)
		return
	}
	out := slaReportDTO{Period: f.period, From: f.from, To: f.to, Bucket: f.bucket, SLAs: []slaReportSLA{}, Trend: []slaReportPoint{},
		Groups: []slaReportBreak{}, Prios: []slaReportBreak{}, Types: []slaReportBreak{}, AtRisk: []slaReportClock{}, Breach: []slaReportClock{},
		Series: []slaReportSeries{}}
	prios := priorityLabels()
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		ctx := r.Context()
		typeNames, err := typeLabels(r, q)
		if err != nil {
			return err
		}
		calName, err := calendarLabels(ctx, q)
		if err != nil {
			return err
		}
		out.Options = h.reportOptions(typeNames, prios)

		ended, err := q.SLAReportEnded(ctx, f.ended(f.from, f.to))
		if err != nil {
			return err
		}
		prev, err := q.SLAReportEnded(ctx, f.ended(f.from.Add(-f.to.Sub(f.from)), f.from))
		if err != nil {
			return err
		}
		var prevEnded, prevMet int64
		for _, row := range prev {
			prevEnded, prevMet = prevEnded+row.Ended, prevMet+row.Met
		}
		out.Summary.PrevMetPct = share(prevMet, prevEnded)

		medians, err := q.SLAReportMedians(ctx, dbgen.SLAReportMediansParams{From: reportTS(f.from), To: reportTS(f.to), Type: f.typ,
			App: f.app, Sla: f.sla, GroupID: f.group, NoGroup: f.noGroup, Priority: f.priority})
		if err != nil {
			return err
		}
		open, err := q.SLAReportOpen(ctx, dbgen.SLAReportOpenParams{Type: f.typ, App: f.app, Sla: f.sla, GroupID: f.group,
			NoGroup: f.noGroup, Priority: f.priority, Max: maxOpenClocks})
		if err != nil {
			return err
		}
		out.Partial = len(open) == maxOpenClocks
		breaches, err := q.SLAReportBreaches(ctx, dbgen.SLAReportBreachesParams{From: reportTS(f.from), To: reportTS(f.to), Type: f.typ,
			App: f.app, Sla: f.sla, GroupID: f.group, NoGroup: f.noGroup, Priority: f.priority, Max: maxReportRows})
		if err != nil {
			return err
		}
		cals := slaclock.NewCalendars(q)

		// Ended clocks: the summary, each SLA, the trend and the breakdowns.
		perSLA := map[string]*slaReportSLA{}
		groups, priorities, types := breakdown{}, breakdown{}, breakdown{}
		points := map[int64]map[string]*slaReportCounted{} // by bucket start, in Unix seconds
		for _, row := range ended {
			key := row.App + "/" + row.Sla
			out.Summary.Ended += row.Ended
			out.Summary.Met += row.Met
			s := perSLA[key]
			if s == nil {
				s = &slaReportSLA{Key: key, Label: row.Label}
				perSLA[key] = s
			}
			s.Ended, s.Met = s.Ended+row.Ended, s.Met+row.Met
			at := row.Bucket.Time.Unix()
			if points[at] == nil {
				points[at] = map[string]*slaReportCounted{}
			}
			c := points[at][key]
			if c == nil {
				c = &slaReportCounted{}
				points[at][key] = c
			}
			c.Ended, c.Met = c.Ended+row.Ended, c.Met+row.Met
			groups.add(groupKey(row.AssignmentGroupID), groupLabel(row.GroupName), row.Ended, row.Met, 0)
			priorities.add(strconv.Itoa(int(row.Priority)), prios[row.Priority], row.Ended, row.Met, 0)
			types.add(row.Type, typeNames[row.Type], row.Ended, row.Met, 0)
		}
		out.Summary.Breached = out.Summary.Ended - out.Summary.Met
		out.Summary.MetPct = share(out.Summary.Met, out.Summary.Ended)
		for _, m := range medians {
			key := m.App + "/" + m.Sla
			s := perSLA[key]
			if s == nil {
				continue
			}
			med, target := int64(m.MedianMs), int64(m.MedianTargetMs)
			s.MedianMS, s.MedianTargetMS = &med, &target
		}

		// Running and paused clocks: what is still running, and what is at risk.
		var risk []slaReportClock
		for _, row := range open {
			groups.add(groupKey(row.AssignmentGroupID), groupLabel(row.GroupName), 0, 0, 1)
			priorities.add(strconv.Itoa(int(row.Priority)), prios[row.Priority], 0, 0, 1)
			types.add(row.Type, typeNames[row.Type], 0, 0, 1)
			if perSLA[row.App+"/"+row.Sla] == nil {
				perSLA[row.App+"/"+row.Sla] = &slaReportSLA{Key: row.App + "/" + row.Sla, Label: row.Label}
			}
			out.Summary.Running++
			if row.Stage != slaclock.Running || row.BreachedAt.Valid || row.TargetMs <= 0 {
				continue
			}
			cal, _, err := cals.Get(ctx, row.Calendar)
			if err != nil {
				return err
			}
			c := slaclock.Clock{Stage: row.Stage, Elapsed: time.Duration(row.ElapsedMs) * time.Millisecond, RunningSince: row.RunningSince.Time}
			used := c.Used(now, cal).Milliseconds()
			if float64(used) < atRiskShare*float64(row.TargetMs) {
				continue
			}
			rc := slaReportClock{Reference: row.Reference, Title: row.Title, Link: "/tickets/" + row.Reference, SLA: row.Label,
				Group: groupLabel(row.GroupName), UsedMS: used, TargetMS: row.TargetMs, UsedPct: int(used * 100 / row.TargetMs),
				Calendar: calName(row.Calendar)}
			if row.DueAt.Valid {
				rc.DueAt = &row.DueAt.Time
			}
			risk = append(risk, rc)
		}
		out.Summary.AtRisk = len(risk)
		sort.SliceStable(risk, func(i, j int) bool { return risk[i].UsedMS*risk[j].TargetMS > risk[j].UsedMS*risk[i].TargetMS })
		out.AtRisk = append(out.AtRisk, risk[:min(len(risk), maxReportRows)]...)

		for _, row := range breaches {
			used := row.ElapsedMs
			if row.Stage == slaclock.Running {
				cal, _, err := cals.Get(ctx, row.Calendar)
				if err != nil {
					return err
				}
				c := slaclock.Clock{Stage: row.Stage, Elapsed: time.Duration(row.ElapsedMs) * time.Millisecond, RunningSince: row.RunningSince.Time}
				used = c.Used(now, cal).Milliseconds()
			}
			bc := slaReportClock{Reference: row.Reference, Title: row.Title, Link: "/tickets/" + row.Reference, SLA: row.Label,
				Group: groupLabel(row.GroupName), UsedMS: used, TargetMS: row.TargetMs, OverMS: max(0, used-row.TargetMs),
				BreachedAt: &row.BreachedAt.Time, Calendar: calName(row.Calendar)}
			if row.TargetMs > 0 {
				bc.UsedPct = int(used * 100 / row.TargetMs)
			}
			out.Breach = append(out.Breach, bc)
		}

		// Each SLA, by label; the trend has a point for every bucket in the period, empty ones included.
		for _, s := range perSLA {
			s.MetPct = share(s.Met, s.Ended)
			out.SLAs = append(out.SLAs, *s)
		}
		sort.Slice(out.SLAs, func(i, j int) bool { return out.SLAs[i].Label < out.SLAs[j].Label })
		for _, s := range out.SLAs {
			if s.Ended > 0 {
				out.Series = append(out.Series, slaReportSeries{Key: s.Key, Label: s.Label})
			}
		}
		for _, start := range buckets(f.bucket, f.from, f.to) {
			pt := slaReportPoint{Start: start, Values: []slaReportCounted{}}
			for _, s := range out.Series {
				c := slaReportCounted{}
				if x := points[start.Unix()][s.Key]; x != nil {
					c = *x
				}
				c.MetPct = share(c.Met, c.Ended)
				pt.Values = append(pt.Values, c)
			}
			out.Trend = append(out.Trend, pt)
		}
		out.Groups, out.Prios, out.Types = groups.rows(true), priorities.rows(false), types.rows(true)
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// breakdown adds up clocks by one property.
type breakdown map[string]*slaReportBreak

func (b breakdown) add(key, label string, ended, met, running int64) {
	row := b[key]
	if row == nil {
		row = &slaReportBreak{Key: key, Label: label}
		b[key] = row
	}
	row.Ended, row.Met, row.Breached, row.Running = row.Ended+ended, row.Met+met, row.Breached+ended-met, row.Running+running
}

// rows lists a breakdown: the busiest first, or by key (priorities, 1 first).
func (b breakdown) rows(busiest bool) []slaReportBreak {
	out := make([]slaReportBreak, 0, len(b))
	for _, row := range b {
		row.MetPct = share(row.Met, row.Ended)
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if busiest {
			a, c := out[i].Ended+out[i].Running, out[j].Ended+out[j].Running
			if a != c {
				return a > c
			}
			return out[i].Label < out[j].Label
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// buckets are the starts of the day, week (Monday) or month buckets that [from, to) touches, in UTC, as Postgres's
// date_trunc gives them.
func buckets(unit string, from, to time.Time) []time.Time {
	start := from.UTC()
	switch unit {
	case "day":
		start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	case "week":
		start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
		start = start.AddDate(0, 0, -((int(start.Weekday()) + 6) % 7))
	default:
		start = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	var out []time.Time
	for t := start; t.Before(to); {
		out = append(out, t)
		switch unit {
		case "day":
			t = t.AddDate(0, 0, 1)
		case "week":
			t = t.AddDate(0, 0, 7)
		default:
			t = t.AddDate(0, 1, 0)
		}
	}
	return out
}

// reportOptions are the filters' choices: the SLAs of the types in use, the types with SLAs, and the priorities.
func (h *handlers) reportOptions(typeNames map[string]string, prios map[int16]string) slaReportOptions {
	out := slaReportOptions{SLAs: []slaReportSeries{}, Types: []slaReportSeries{}, Priorities: []slaReportSeries{}}
	for _, t := range h.apps.RecordTypes() {
		defs := h.apps.SLAs(t)
		if len(defs) == 0 || typeNames[t] == "" {
			continue
		}
		out.Types = append(out.Types, slaReportSeries{Key: t, Label: typeNames[t]})
		for _, d := range defs {
			label := d.Label
			if len(h.apps.RecordTypes()) > 1 {
				label = fmt.Sprintf("%s (%s)", d.Label, typeNames[t])
			}
			out.SLAs = append(out.SLAs, slaReportSeries{Key: d.App + "/" + d.Name, Label: label})
		}
	}
	for n := int16(1); n <= 4; n++ {
		out.Priorities = append(out.Priorities, slaReportSeries{Key: strconv.Itoa(int(n)), Label: prios[n]})
	}
	return out
}

// GET /reports/slas/export (reports:read) with the report's filters: the clocks behind the report as a CSV file, one
// row each: those that ended or breached in the period and those running or paused now, with working time in hours.
func (h *handlers) slaReportExport(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	f, err := readReportFilter(r.URL.Query(), now)
	if err != nil {
		writeDBError(w, err)
		return
	}
	prios := priorityLabels()
	out := [][]string{{"Record", "Title", "Type", "Priority", "Assignment group", "SLA", "Stage", "Started", "Ended", "Target (hours)",
		"Used (hours)", "Met", "Breached at", "Hours counted"}}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		ctx := r.Context()
		typeNames, err := typeLabels(r, q)
		if err != nil {
			return err
		}
		calName, err := calendarLabels(ctx, q)
		if err != nil {
			return err
		}
		rows, err := q.SLAReportClocks(ctx, dbgen.SLAReportClocksParams{From: reportTS(f.from), To: reportTS(f.to), Type: f.typ,
			App: f.app, Sla: f.sla, GroupID: f.group, NoGroup: f.noGroup, Priority: f.priority, Max: maxReportExport})
		if err != nil {
			return err
		}
		cals := slaclock.NewCalendars(q)
		stamp := func(t pgtype.Timestamptz) string {
			if !t.Valid {
				return ""
			}
			return t.Time.UTC().Format(time.RFC3339)
		}
		hours := func(ms int64) string { return strconv.FormatFloat(float64(ms)/3.6e6, 'f', 2, 64) }
		for _, row := range rows {
			used := row.ElapsedMs
			if row.Stage == slaclock.Running {
				cal, _, err := cals.Get(ctx, row.Calendar)
				if err != nil {
					return err
				}
				c := slaclock.Clock{Stage: row.Stage, Elapsed: time.Duration(row.ElapsedMs) * time.Millisecond, RunningSince: row.RunningSince.Time}
				used = c.Used(now, cal).Milliseconds()
			}
			met := ""
			switch {
			case row.BreachedAt.Valid:
				met = "no"
			case row.Stage == slaclock.Stopped:
				met = "yes"
			}
			typ := typeNames[row.Type]
			if typ == "" {
				typ = row.Type
			}
			out = append(out, []string{row.Reference, row.Title, typ, prios[row.Priority], row.GroupName, row.Label, row.Stage,
				stamp(row.StartedAt), stamp(row.StoppedAt), hours(row.TargetMs), hours(used), met, stamp(row.BreachedAt), calName(row.Calendar)})
		}
		return nil
	}) {
		return
	}
	csvHeaders(w, "sla-report-"+f.period+".csv")
	w.WriteHeader(http.StatusOK)
	_ = cmdb.WriteCSV(w, out) // too late to answer otherwise
}
