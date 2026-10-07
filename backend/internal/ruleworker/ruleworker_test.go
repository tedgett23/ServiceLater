package ruleworker_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/ruleworker"
)

type world struct {
	pool      *pgxpool.Pool
	reg       *apps.Registry
	agent     uuid.UUID
	requester uuid.UUID
	ticket    uuid.UUID
}

func setup(t *testing.T, rule string) *world {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.InstallFirstParty(t, pool)
	ctx := context.Background()
	w := &world{pool: pool}
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		a, err := q.CreateUser(ctx, dbgen.CreateUserParams{Email: "agent@x.test", Name: "Ada", Role: "agent"})
		if err != nil {
			return err
		}
		r, err := q.CreateUser(ctx, dbgen.CreateUserParams{Email: "lena@x.test", Name: "Lena", Role: "end_user"})
		if err != nil {
			return err
		}
		w.agent, w.requester = a.ID, r.ID
		task, err := q.CreateTask(ctx, dbgen.CreateTaskParams{Type: "incident", Title: "Printer", Priority: 3, RequesterID: r.ID, Status: "new", StateCategory: "open"})
		if err != nil {
			return err
		}
		w.ticket = task.ID
		return q.CreateIncident(ctx, task.ID)
	}); err != nil {
		t.Fatal(err)
	}
	w.installRule(t, rule)
	return w
}

func (w *world) installRule(t *testing.T, rule string) {
	t.Helper()
	w.installRuleWith(t, rule, "[records:read:*, records:update:*, comments:internal]")
}

// installRuleWith is installRule with the app's own manifest permissions.
func (w *world) installRuleWith(t *testing.T, rule, perms string) {
	t.Helper()
	files := map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: house-rules\nversion: 1.0.0\nauthor: t\ndescription: \"Rules.\"\npermissions: " + perms + "\n")}
	if rule != "" {
		files["default/rules/x.yaml"] = []byte(rule)
	}
	c, err := (&apps.Bundle{Files: files}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Install(context.Background(), w.pool, c, apps.InstallOptions{ApprovePermissions: true}); err != nil {
		t.Fatal(err)
	}
	w.reg = apps.NewRegistry(w.pool)
	if err := w.reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func rule(internal string) string {
	return "kind: rule\nname: note\nrecord_type: incident\ntiming: async\non: [update]\nrun:\n  - action: comment.add\n    body: =user.name + \" touched \" + record.reference\n    internal: " + internal + "\n"
}

func (w *world) job(user uuid.UUID) jobs.Job {
	raw, _ := json.Marshal(ruleruns.Payload{App: "house-rules", Rule: "note", RecordID: w.ticket, Event: "update", UserID: user})
	return jobs.Job{ID: 7, Kind: ruleruns.Kind, Payload: raw, Attempt: 2, MaxAttempts: 5}
}

func (w *world) run(user uuid.UUID) error {
	return ruleworker.Handler(w.pool, w.reg)(context.Background(), w.job(user))
}

func (w *world) comments(t *testing.T) []dbgen.ListCommentsRow {
	t.Helper()
	var out []dbgen.ListCommentsRow
	if err := db.WithSystem(context.Background(), w.pool, func(tx pgx.Tx) (err error) {
		out, err = dbgen.New(tx).ListComments(context.Background(), w.ticket)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func (w *world) audit(t *testing.T) []dbgen.ActionRun {
	t.Helper()
	var out []dbgen.ActionRun
	if err := db.WithSystem(context.Background(), w.pool, func(tx pgx.Tx) (err error) {
		out, err = dbgen.New(tx).ListActionRuns(context.Background(), dbgen.ListActionRunsParams{Limit: 50})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// An async rule runs as its app, for the person whose change set it off: the comment is the app's, says whom it was
// for, and `user` in the rule is still that person.
func TestARunWritesItsCommentAsTheAppForTheActorAndIsAudited(t *testing.T) {
	w := setup(t, rule("true"))
	if err := w.run(w.agent); err != nil {
		t.Fatal(err)
	}
	cs := w.comments(t)
	if len(cs) != 1 || cs[0].TaskComment.Body != "Ada touched INC1" || !cs[0].TaskComment.Internal || cs[0].AuthorName != "house-rules" ||
		cs[0].OnBehalfOfName.String != "Ada" {
		t.Fatalf("comments: %+v", cs)
	}
	a := w.audit(t)
	if len(a) != 1 || a[0].Outcome != ruleruns.OK || a[0].App != "house-rules" || a[0].Rule != "note" || a[0].Timing != "async" ||
		a[0].RecordID.UUID != w.ticket || !a[0].JobID.Valid || a[0].JobID.Int64 != 7 || a[0].Attempt != 2 {
		t.Errorf("audit: %+v", a)
	}
}

// A ticket that has gone, or that the app may not see, is nothing to retry: the run is recorded as skipped.
func TestATicketThatIsGoneIsSkippedNotRetried(t *testing.T) {
	w := setup(t, rule("true"))
	// An app without records:read:* cannot see Lena's ticket when Ada's change sets it off, so for it the ticket is
	// as good as gone.
	w.installRuleWith(t, rule("true"), "[users:read]")
	if err := w.run(w.agent); err != nil {
		t.Fatalf("a hidden ticket must finish the job, not fail it: %v", err)
	}
	w.installRule(t, rule("true"))
	if err := db.WithSystem(context.Background(), w.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `DELETE FROM tasks WHERE id = $1`, w.ticket)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.run(w.agent); err != nil {
		t.Fatalf("a deleted ticket must finish the job: %v", err)
	}
	a := w.audit(t)
	if len(a) != 2 || a[0].Outcome != ruleruns.Skipped || a[1].Outcome != ruleruns.Skipped {
		t.Errorf("audit: %+v", a)
	}
}

func TestAnUninstalledRuleIsSkipped(t *testing.T) {
	w := setup(t, rule("true"))
	w.installRule(t, "") // the app stays, with no rules
	if err := w.run(w.agent); err != nil {
		t.Fatal(err)
	}
	if a := w.audit(t); len(a) != 1 || a[0].Outcome != ruleruns.Skipped || len(w.comments(t)) != 0 {
		t.Errorf("audit: %+v", a)
	}
}

// An async rule may do what its app's manifest declares, whoever set it off: Lena, a requester, sets off an internal
// note through an app that may write them. An app that may not is refused by the database, recorded, and
// dead-lettered at once: retrying cannot change its manifest.
func TestARunCannotDoMoreThanItsAppMay(t *testing.T) {
	w := setup(t, rule("true"))
	if err := w.run(w.requester); err != nil {
		t.Fatalf("an app that may write internal notes, set off by a requester: %v", err)
	}
	if cs := w.comments(t); len(cs) != 1 || !cs[0].TaskComment.Internal {
		t.Fatalf("comments: %+v", cs)
	}
	if _, err := w.pool.Exec(context.Background(), "DELETE FROM task_comments; DELETE FROM action_runs"); err != nil {
		t.Fatal(err)
	}
	w.installRuleWith(t, rule("true"), "[records:read:*]")
	err := w.run(w.requester)
	if !jobs.IsPermanent(err) {
		t.Fatalf("want a permanent failure, got %v", err)
	}
	if len(w.comments(t)) != 0 {
		t.Error("the refused comment was written")
	}
	if a := w.audit(t); len(a) != 1 || a[0].Outcome != ruleruns.Failed || a[0].Detail == "" {
		t.Errorf("audit: %+v", a)
	}
	// The same rule with a public comment is within what the app may.
	w.installRuleWith(t, rule("false"), "[records:read:*]")
	if err := w.run(w.requester); err != nil {
		t.Fatal(err)
	}
	if cs := w.comments(t); len(cs) != 1 || cs[0].TaskComment.Internal {
		t.Errorf("comments: %+v", cs)
	}
}

// What the data makes an expression fail is retried: it may succeed once the data changes. Nothing is half written.
func TestAnExpressionErrorIsRetriedAndWritesNothing(t *testing.T) {
	w := setup(t, "kind: rule\nname: note\nrecord_type: incident\ntiming: async\non: [update]\nrun:\n"+
		"  - action: comment.add\n    body: first\n"+
		"  - action: comment.add\n    body: '=string(record.priority / (record.priority - 3))'\n")
	err := w.run(w.agent)
	if err == nil || jobs.IsPermanent(err) {
		t.Fatalf("want a retryable error, got %v", err)
	}
	if len(w.comments(t)) != 0 {
		t.Error("the first comment survived a failed run")
	}
	if a := w.audit(t); len(a) != 1 || a[0].Outcome != ruleruns.Failed {
		t.Errorf("audit: %+v", a)
	}
}

func TestAnUnreadablePayloadIsDeadLetteredAtOnce(t *testing.T) {
	w := setup(t, rule("true"))
	err := ruleworker.Handler(w.pool, w.reg)(context.Background(), jobs.Job{ID: 1, Payload: []byte(`{"record_id": 5}`), Attempt: 1})
	if !jobs.IsPermanent(err) {
		t.Errorf("got %v", err)
	}
}

func TestQueueingRollsBackWithTheSave(t *testing.T) {
	w := setup(t, rule("true"))
	ctx := context.Background()
	fired := []*apps.Rule{w.reg.Rule("house-rules", "note")}
	count := func() int {
		var n int
		_ = db.WithSystem(ctx, w.pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind = 'rule.async'`).Scan(&n)
		})
		return n
	}
	boom := errors.New("the save failed")
	err := db.WithUser(ctx, w.pool, w.agent, func(tx pgx.Tx) error {
		if err := ruleruns.Enqueue(ctx, dbgen.New(tx), w.pool, fired, "update", w.ticket, w.agent, ruleruns.Cause{}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || count() != 0 {
		t.Fatalf("a rolled back save queued %d jobs (%v)", count(), err)
	}
	if err := db.WithUser(ctx, w.pool, w.agent, func(tx pgx.Tx) error {
		return ruleruns.Enqueue(ctx, dbgen.New(tx), w.pool, fired, "update", w.ticket, w.agent, ruleruns.Cause{})
	}); err != nil || count() != 1 {
		t.Fatalf("a committed save queued %d jobs (%v)", count(), err)
	}
}
