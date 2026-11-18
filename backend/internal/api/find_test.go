package api_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
)

// What a query step can filter by (apps.QueryFields) is what the database function app_find_records knows.
func TestQueryFieldsMatchTheDatabase(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	sample := map[string]any{"text": "x", "id": uuid.NewString(), "time": time.Now()}
	for _, typ := range apps.CoreRecordTypes {
		fields, ok := apps.QueryFields(typ)
		if !ok {
			continue
		}
		for field, kind := range fields {
			for _, op := range apps.LookupOps {
				v := sample[kind]
				if op == "in" {
					if kind == "time" {
						continue
					}
					v = []string{sample[kind].(string)}
				}
				raw, _ := json.Marshal([]expr.Filter{{Field: field, Op: op, Value: v}})
				err := db.WithUser(context.Background(), pool, uuid.MustParse(admin.id), func(tx pgx.Tx) error {
					_, err := dbgen.New(tx).FindRecords(context.Background(), dbgen.FindRecordsParams{Type: typ, Filters: raw, Max: 5})
					return err
				})
				if err != nil {
					t.Errorf("%s.%s %s: %v", typ, field, op, err)
				}
			}
		}
	}
}
