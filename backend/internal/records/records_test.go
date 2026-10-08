package records_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/records"
)

// A field added to expr.Record must be filled by Ticket.ExprRecord, or rules see it empty in every save.
func TestExprRecordSetsEveryField(t *testing.T) {
	id, other, name, now := uuid.New(), uuid.New(), "Ada", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	r := records.Ticket{
		ID: id, Type: "incident", Number: 4, Reference: "INC4", Title: "t", Description: "d", Status: "new", StateCategory: "open",
		Priority: 2, RequesterID: other, Requester: "R", AssigneeID: &other, Assignee: &name, AssignmentGroupID: &other, AssignmentGroup: &name, CreatedAt: now, UpdatedAt: now, Resolution: "fixed",
		// No real ticket has both an incident's and a change's own fields; this one has, so every field is checked.
		Change: &records.Change{ChangeType: "normal", Risk: "low", Impact: "high", PlannedStart: &now, PlannedEnd: &now,
			ImplementationPlan: "i", BackoutPlan: "b", TestPlan: "t"},
		Fields: map[string]any{"category": "network"},
		CIs: []records.CI{{ID: id, Class: "server", Name: "db-1", Status: "active", SupportGroupID: &other, SupportGroup: &name,
			OwnerGroupID: &other, OwnerGroup: &name}},
	}.ExprRecord()
	v := reflect.ValueOf(*r)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Errorf("expr.Record.%s is not set by Ticket.ExprRecord", v.Type().Field(i).Name)
		}
	}
	ci := reflect.ValueOf(r.CIs[0])
	for i := 0; i < ci.NumField(); i++ {
		if ci.Field(i).IsZero() {
			t.Errorf("expr.CI.%s is not set by Ticket.ExprRecord", ci.Type().Field(i).Name)
		}
	}
}
