package workflowengine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/workflow"
)

// user creates another end user, for tests that need more than the world's built-in agent and requester.
func (w *world) user(t *testing.T, email, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.WithSystem(context.Background(), w.pool, func(tx pgx.Tx) error {
		u, err := dbgen.New(tx).CreateUser(context.Background(), dbgen.CreateUserParams{Email: email, Name: name, Role: "end_user"})
		id = u.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// approvalsFor is a step's requests, decided or not.
func (w *world) approvalsFor(t *testing.T, execID uuid.UUID, stepID string) []dbgen.WorkflowApproval {
	t.Helper()
	var out []dbgen.WorkflowApproval
	if err := db.WithSystem(context.Background(), w.pool, func(tx pgx.Tx) (err error) {
		out, err = dbgen.New(tx).ListApprovalsForStep(context.Background(), dbgen.ListApprovalsForStepParams{ExecutionID: execID, StepID: stepID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func approvalOf(rows []dbgen.WorkflowApproval, approver uuid.UUID) dbgen.WorkflowApproval {
	for _, r := range rows {
		if r.ApproverID == approver {
			return r
		}
	}
	return dbgen.WorkflowApproval{}
}

// decide answers a pending approval as the given approver, under their own RLS, the way the API handler does.
func (w *world) decide(approver, approvalID uuid.UUID, decision, comment string) error {
	return db.WithUser(context.Background(), w.pool, approver, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).DecideApproval(context.Background(), dbgen.DecideApprovalParams{
			ID: approvalID, Decision: pgtype.Text{String: decision, Valid: true}, Comment: comment})
		return err
	})
}

func vars(t *testing.T, ex dbgen.WorkflowExecution) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(ex.Vars, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// approvalFlow asks the given approvers (as a CEL list literal), branches on the decision and leaves a comment saying
// which way it went, so a test can read the outcome from the ticket's comments as well as the execution's vars.
func approvalFlow(rule, approversLiteral string) string {
	r := ""
	if rule != "" {
		r = "\n    rule: " + rule
	}
	return "  ask:\n    type: approval\n    approvers: '" + approversLiteral + "'" + r + "\n    next: after\n  after:\n    type: condition\n" +
		"    if: 'vars.ask_decision == \"approved\"'\n    then: yes\n    else: no\n" +
		"  yes:\n    type: comment.add\n    body: approved\n    next: done\n" +
		"  no:\n    type: comment.add\n    body: rejected\n    next: done\n  done:\n    type: end\n"
}

func TestApprovalAnyRuleApprovesAsSoonAsOneSaysYes(t *testing.T) {
	w := setup(t)
	bob := w.user(t, "bob@x.test", "Bob")
	carol := w.user(t, "carol@x.test", "Carol")
	w.install(t, "house", map[string]string{"ask": wf("ask", "ask",
		approvalFlow("any", `["`+bob.String()+`", "`+carol.String()+`"]`))}, nil)
	id := w.start(t, "house", "ask", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	ex := w.execution(t, id)
	if ex.Status != workflow.Waiting || ex.CurrentStep != "ask" {
		t.Fatalf("should wait for the approvers: %+v", ex)
	}
	rows := w.approvalsFor(t, id, "ask")
	if len(rows) != 2 {
		t.Fatalf("should ask both approvers, asked %d", len(rows))
	}
	bobRow := approvalOf(rows, bob)
	// Not yet: a stray wake finds nothing due, and running does nothing.
	if n, err := w.engine.WakeDue(context.Background()); err != nil || n != 0 {
		t.Fatalf("woke %d, %v", n, err)
	}
	if err := w.decide(bob, bobRow.ID, "approved", "looks fine"); err != nil {
		t.Fatal(err)
	}
	// Deciding should have brought the execution's wake-up forward at once (the trigger), without needing the timer.
	if time.Until(w.execution(t, id).WakeAt.Time) > 2*time.Second {
		t.Fatal("a decision should wake the execution")
	}
	if n, err := w.engine.WakeDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("woke %d, %v", n, err)
	}
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	ex = w.execution(t, id)
	if ex.Status != workflow.Done {
		t.Fatalf("any rule should not wait for carol: %s", ex.Status)
	}
	if v := vars(t, ex); v["ask_decision"] != "approved" || v["ask_comment"] != "looks fine" || v["ask_decided_by"] != "Bob" {
		t.Errorf("vars: %v", v)
	}
	if cs := w.comments(t); len(cs) != 1 || cs[0] != "approved" {
		t.Errorf("comments: %v", cs)
	}
}

func TestApprovalAnyRuleRejectsWhenEveryoneDeclines(t *testing.T) {
	w := setup(t)
	bob := w.user(t, "bob@x.test", "Bob")
	w.install(t, "house", map[string]string{"ask": wf("ask", "ask", approvalFlow("any", `["`+bob.String()+`"]`))}, nil)
	id := w.start(t, "house", "ask", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	row := approvalOf(w.approvalsFor(t, id, "ask"), bob)
	if err := w.decide(bob, row.ID, "rejected", "no"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.WakeDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if cs := w.comments(t); len(cs) != 1 || cs[0] != "rejected" {
		t.Errorf("comments: %v", cs)
	}
}

func TestApprovalAllRuleNeedsEveryoneAndRejectsAtOnceOnAnyNo(t *testing.T) {
	w := setup(t)
	bob := w.user(t, "bob@x.test", "Bob")
	carol := w.user(t, "carol@x.test", "Carol")
	w.install(t, "house", map[string]string{"ask": wf("ask", "ask",
		approvalFlow("all", `["`+bob.String()+`", "`+carol.String()+`"]`))}, nil)
	id := w.start(t, "house", "ask", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	rows := w.approvalsFor(t, id, "ask")
	bobRow, carolRow := approvalOf(rows, bob), approvalOf(rows, carol)
	// Bob approves: not enough on its own for "all".
	if err := w.decide(bob, bobRow.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.WakeDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if ex := w.execution(t, id); ex.Status != workflow.Waiting {
		t.Fatalf("one of two should still wait: %s", ex.Status)
	}
	// Carol declines: "all" rejects at once, it does not wait for a majority.
	if err := w.decide(carol, carolRow.ID, "rejected", "no"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.WakeDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if cs := w.comments(t); len(cs) != 1 || cs[0] != "rejected" {
		t.Errorf("comments: %v", cs)
	}
}

func TestApprovalMajorityNeedsMoreThanHalf(t *testing.T) {
	w := setup(t)
	bob := w.user(t, "bob@x.test", "Bob")
	carol := w.user(t, "carol@x.test", "Carol")
	dave := w.user(t, "dave@x.test", "Dave")
	w.install(t, "house", map[string]string{"ask": wf("ask", "ask",
		approvalFlow("majority", `["`+bob.String()+`", "`+carol.String()+`", "`+dave.String()+`"]`))}, nil)
	id := w.start(t, "house", "ask", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	rows := w.approvalsFor(t, id, "ask")
	bobRow, carolRow, daveRow := approvalOf(rows, bob), approvalOf(rows, carol), approvalOf(rows, dave)
	// One of three approving is not a majority.
	if err := w.decide(bob, bobRow.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.WakeDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if ex := w.execution(t, id); ex.Status != workflow.Waiting {
		t.Fatalf("one of three should not be a majority: %s", ex.Status)
	}
	// A second approval makes two of three: a majority, decided without waiting for dave.
	if err := w.decide(carol, carolRow.ID, "approved", "fine by me"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.WakeDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if ex := w.execution(t, id); ex.Status != workflow.Done {
		t.Fatalf("two of three should be a majority: %s", ex.Status)
	}
	if cs := w.comments(t); len(cs) != 1 || cs[0] != "approved" {
		t.Errorf("comments: %v", cs)
	}
	// Dave never having answered is fine: the row is just left pending.
	if daveRow.Decision.Valid {
		t.Error("dave's row should not have been touched")
	}
}

func TestApprovalOnlyTheApproverCanDecideAndOnlyOnce(t *testing.T) {
	w := setup(t)
	bob := w.user(t, "bob@x.test", "Bob")
	stranger := w.user(t, "stranger@x.test", "Stranger")
	w.install(t, "house", map[string]string{"ask": wf("ask", "ask", approvalFlow("any", `["`+bob.String()+`"]`))}, nil)
	id := w.start(t, "house", "ask", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	row := approvalOf(w.approvalsFor(t, id, "ask"), bob)
	if err := w.decide(stranger, row.ID, "approved", ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("someone else deciding should find nothing: %v", err)
	}
	if err := w.decide(bob, row.ID, "approved", "ok"); err != nil {
		t.Fatal(err)
	}
	if err := w.decide(bob, row.ID, "rejected", "changed my mind"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deciding twice should find nothing: %v", err)
	}
}

func TestApprovalTimeoutTakesItsBranchOrEndsTheRun(t *testing.T) {
	w := setup(t)
	bob := w.user(t, "bob@x.test", "Bob")
	w.install(t, "house", map[string]string{
		"soft": wf("soft", "ask", "  ask:\n    type: approval\n    approvers: '[\""+bob.String()+"\"]'\n    timeout: 1h\n    on_timeout: gone\n    next: done\n  gone:\n    type: comment.add\n    body: nobody answered\n    next: done\n  done:\n    type: end\n"),
		"hard": wf("hard", "ask", "  ask:\n    type: approval\n    approvers: '[\""+bob.String()+"\"]'\n    timeout: 1h\n    next: done\n  done:\n    type: end\n"),
	}, nil)
	for _, name := range []string{"soft", "hard"} {
		id := w.start(t, "house", name, w.agent)
		if err := w.run(id); err != nil {
			t.Fatal(err)
		}
		w.exec(t, `UPDATE workflow_executions SET wait_deadline = now() - interval '1 second', wake_at = now() WHERE id = $1`, id)
		if n, err := w.engine.WakeDue(context.Background()); err != nil || n != 1 {
			t.Fatalf("woke %d, %v", n, err)
		}
		if err := w.run(id); err != nil {
			t.Fatal(err)
		}
		ex := w.execution(t, id)
		if name == "soft" {
			if ex.Status != workflow.Done || !strings.Contains(stepIDs(w.steps(t, id)), "ask:timeout gone:ok") {
				t.Errorf("on_timeout: %s %s", ex.Status, stepIDs(w.steps(t, id)))
			}
			if v := vars(t, ex); v["ask_decision"] != "timeout" {
				t.Errorf("vars: %v", v)
			}
		} else if ex.Status != workflow.Failed || !strings.Contains(ex.Error, "timed out after 1h") {
			t.Errorf("no on_timeout should fail: %s %q", ex.Status, ex.Error)
		}
	}
}

func TestApprovalRejectsABadApproverList(t *testing.T) {
	w := setup(t)
	unknown := uuid.New()
	w.install(t, "house", map[string]string{
		"notauser": wf("notauser", "ask", "  ask:\n    type: approval\n    approvers: '[\"not-a-uuid\"]'\n    next: done\n  done:\n    type: end\n"),
		"noone":    wf("noone", "ask", "  ask:\n    type: approval\n    approvers: '[record.requester_id].filter(x, x == \"nope\")'\n    next: done\n  done:\n    type: end\n"),
		"ghost":    wf("ghost", "ask", "  ask:\n    type: approval\n    approvers: '[\""+unknown.String()+"\"]'\n    next: done\n  done:\n    type: end\n"),
	}, nil)
	cases := []struct{ name, want string }{
		{"notauser", "is not a user id"},
		{"noone", "no approvers"},
		{"ghost", "no such user"},
	}
	for _, c := range cases {
		id := w.start(t, "house", c.name, w.agent)
		if err := w.run(id); err != nil {
			t.Fatal(err)
		}
		if ex := w.execution(t, id); ex.Status != workflow.Failed || !strings.Contains(ex.Error, c.want) {
			t.Errorf("%s: %s %q", c.name, ex.Status, ex.Error)
		}
	}
}
