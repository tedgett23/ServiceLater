package api_test

import (
	"strings"
	"testing"
	"time"
)

type reportBody struct {
	Summary struct {
		Ended, Met, Breached int64
		MetPct               *float64 `json:"met_pct"`
		Running              int
		AtRisk               int `json:"at_risk"`
	} `json:"summary"`
	SLAs []struct {
		Key      string
		Label    string
		Ended    int64
		MedianMS *int64 `json:"median_ms"`
	} `json:"slas"`
	Series []struct{ Key, Label string } `json:"series"`
	Trend  []struct {
		Start  time.Time
		Values []struct{ Ended, Met int64 }
	} `json:"trend"`
	Groups []struct {
		Key, Label               string
		Ended, Breached, Running int64
	} `json:"groups"`
	AtRisk []struct {
		Reference string
		Link      string
		UsedPct   int `json:"used_pct"`
		Calendar  string
	} `json:"at_risk"`
	Breaches []struct {
		Reference string
		OverMS    int64 `json:"over_ms"`
	} `json:"breaches"`
	Options struct {
		SLAs []struct{ Key, Label string }
	} `json:"options"`
}

// The SLA report counts clocks as the reader sees them: met and breached in the period, running and at risk now, by
// SLA, over time and by team; a restricted team's records count only for those who may see them.
func TestSLAReport(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	everyHour(t, h, admin)
	desk := makeGroup(t, h, admin, "Service Desk")
	hr := decodeBody[struct{ ID string }](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"HR","restricted":true}`)).ID

	file := func(who actor, title, group string) slaTicket {
		t.Helper()
		return decodeBody[slaTicket](t, who.do(h, "POST", "/api/v1/tickets", `{"title":"`+title+`","priority":2,"assignment_group_id":"`+group+`"}`))
	}
	resolve := func(who actor, tk slaTicket) {
		t.Helper()
		for _, to := range []string{"in_progress", "resolved"} {
			expect(t, who.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/transitions", `{"to":"`+to+`"}`), 200, "moving "+tk.Reference+" to "+to)
		}
	}
	met := file(agent, "Met in time", desk)
	resolve(agent, met)
	late := file(agent, "Resolved late", desk)
	backdate(t, pool, late.ID, "resolve", 9*time.Hour) // P2 resolves in 8h
	resolve(agent, late)
	risky := file(agent, "Nearly due", desk)
	backdate(t, pool, risky.ID, "resolve", 7*time.Hour)
	secret := file(admin, "Payroll question", hr)
	backdate(t, pool, secret.ID, "resolve", 9*time.Hour)
	resolve(admin, secret)

	report := func(who actor, query string) reportBody {
		t.Helper()
		return decodeBody[reportBody](t, who.do(h, "GET", "/api/v1/reports/slas"+query, ""))
	}
	r := report(agent, "")
	s := r.Summary
	if s.Ended != 4 || s.Met != 3 || s.Breached != 1 || s.MetPct == nil || *s.MetPct != 75 || s.Running != 2 || s.AtRisk != 1 {
		t.Errorf("summary as the agent: %+v", s)
	}
	if len(r.AtRisk) != 1 || r.AtRisk[0].Reference != risky.Reference || r.AtRisk[0].Link != "/tickets/"+risky.Reference ||
		r.AtRisk[0].UsedPct < 85 || r.AtRisk[0].Calendar != "Business hours" {
		t.Errorf("at risk: %+v", r.AtRisk)
	}
	if len(r.Breaches) != 1 || r.Breaches[0].Reference != late.Reference || !near(time.Duration(r.Breaches[0].OverMS)*time.Millisecond, time.Hour, time.Minute) {
		t.Errorf("breaches: %+v", r.Breaches)
	}
	if len(r.Series) != 2 || r.Series[0].Label != "Resolve" || r.Series[1].Label != "Respond" || len(r.Trend) < 5 {
		t.Fatalf("trend: %+v %d points", r.Series, len(r.Trend))
	}
	var resolved int64
	for _, p := range r.Trend {
		resolved += p.Values[0].Ended
	}
	if resolved != 2 {
		t.Errorf("the trend counts the two resolve clocks that ended: %d", resolved)
	}
	if len(r.Groups) != 1 || r.Groups[0].Label != "Service Desk" || r.Groups[0].Ended != 4 || r.Groups[0].Breached != 1 || r.Groups[0].Running != 2 {
		t.Errorf("by group: %+v", r.Groups)
	}
	for _, x := range r.SLAs {
		if x.Label == "Resolve" && (x.Ended != 2 || x.MedianMS == nil) {
			t.Errorf("resolve: %+v", x)
		}
	}
	if len(r.Options.SLAs) < 2 {
		t.Errorf("options: %+v", r.Options)
	}

	// The admin may see HR's records, so they count for them.
	if a := report(admin, ""); a.Summary.Ended != 6 || a.Summary.Breached != 2 || len(a.Breaches) != 2 {
		t.Errorf("as the admin: %+v %+v", a.Summary, a.Breaches)
	}
	if x := report(agent, "?sla=incident-management/resolve"); x.Summary.Ended != 2 || x.Summary.Running != 1 {
		t.Errorf("one SLA: %+v", x.Summary)
	}
	if x := report(agent, "?group=none&period=7d"); x.Summary.Ended != 0 || len(x.Trend) < 7 {
		t.Errorf("no group: %+v, %d days", x.Summary, len(x.Trend))
	}
	if x := report(agent, "?priority=1&period=12m"); x.Summary.Ended != 0 {
		t.Errorf("P1: %+v", x.Summary)
	}
	if x := report(agent, "?group="+desk+"&type=incident"); x.Summary.Ended != 4 {
		t.Errorf("the desk's incidents: %+v", x.Summary)
	}
	refusal(t, agent.do(h, "GET", "/api/v1/reports/slas?period=forever", ""), 422, "period must be", "an unknown period")
	refusal(t, agent.do(h, "GET", "/api/v1/reports/slas?group=desk", ""), 422, "group must be", "a group that is not an id")
	expect(t, lena.do(h, "GET", "/api/v1/reports/slas", ""), 403, "an end user")

	res := agent.do(h, "GET", "/api/v1/reports/slas/export", "")
	expect(t, res, 200, "the export")
	csv := res.Body.String()
	lines := strings.Split(strings.TrimSpace(strings.TrimPrefix(csv, "\uFEFF")), "\n")
	if len(lines) != 7 || !strings.HasPrefix(lines[0], "Record,Title,Type,Priority") || !strings.Contains(csv, risky.Reference) || strings.Contains(csv, secret.Reference) {
		t.Errorf("export (%d lines):\n%s", len(lines), csv)
	}
	expect(t, lena.do(h, "GET", "/api/v1/reports/slas/export", ""), 403, "an end user exporting")
}
