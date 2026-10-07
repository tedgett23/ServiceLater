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
	}.ExprRecord()
	v := reflect.ValueOf(*r)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Errorf("expr.Record.%s is not set by Ticket.ExprRecord", v.Type().Field(i).Name)
		}
	}
}
