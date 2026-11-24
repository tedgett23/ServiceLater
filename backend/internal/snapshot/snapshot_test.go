package snapshot_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/snapshot"
)

func keyring(t *testing.T, b byte, old ...[]byte) *secrets.Keyring {
	t.Helper()
	k, err := secrets.NewKeyring(bytes.Repeat([]byte{b}, 32), old...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// fingerprint is every snapshotted table's rows, as text, in a stable order: two databases with the same
// fingerprint hold the same data.
func fingerprint(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT relname FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relkind = 'r'
		AND relname NOT IN ('sessions', 'rate_limits', 'rate_counts', 'jobs', 'goose_db_version')`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	out := map[string]string{}
	for _, tbl := range tables {
		var fp string
		if err := pool.QueryRow(ctx, `SELECT coalesce(md5(string_agg(x::text, '|' ORDER BY x::text)), '') FROM `+tbl+` x`).Scan(&fp); err != nil {
			t.Fatal(err)
		}
		out[tbl] = fp
	}
	var seqs string
	if err := pool.QueryRow(ctx, `SELECT string_agg(sequencename || '=' || coalesce(last_value::text, '-'), ',' ORDER BY sequencename)
		FROM pg_sequences WHERE schemaname = 'public' AND sequencename LIKE 'task_number_%'`).Scan(&seqs); err != nil {
		t.Fatal(err)
	}
	out["(numbering)"] = seqs
	return out
}

func installTyler(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	b, err := apps.Load(filepath.Join(filepath.Dir(file), "..", "apps", "testdata", "tyler"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Install(context.Background(), pool, c, apps.InstallOptions{ApprovePermissions: true}); err != nil {
		t.Fatal(err)
	}
}

func seed(t *testing.T, pool *pgxpool.Pool) (user, task string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, name, role) VALUES ('pat@x.test', 'Pat', 'agent') RETURNING id`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO tasks (type, title, requester_id, assignee_id) VALUES ('incident', 'Disk full', $1, $1) RETURNING id`, user).Scan(&task); err != nil {
		t.Fatal(err)
	}
	// A record of an app's own type when the sample app is installed, else a second incident.
	if _, err := pool.Exec(ctx, `INSERT INTO tasks (type, title, requester_id)
		SELECT coalesce((SELECT type FROM task_types WHERE type = 'tyler_thing'), 'incident'), 'A thing', $1`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_comments (task_id, author_id, body) VALUES ($1, $2, 'On it')`, task, user); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO attachments (task_id, filename, content_type, size, backend, key, part_size, status, created_by)
		VALUES ($1, 'log.txt', 'text/plain', 5, 'local', 'attachments/ab/log', 5, 'ready', $2)`, task, user); err != nil {
		t.Fatal(err)
	}
	return user, task
}

// A snapshot taken, then the database changed, then the snapshot restored: every table, every record number and the
// attachment files are as they were, history was not written by the loading, and numbering carries on.
func TestASnapshotRestoresEverything(t *testing.T) {
	pool := dbtest.Pool(t)
	dbtest.InstallFirstParty(t, pool)
	installTyler(t, pool)
	ctx := context.Background()
	user, task := seed(t, pool)
	files := t.TempDir()
	if err := os.MkdirAll(filepath.Join(files, "attachments", "ab"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(files, "attachments", "ab", "log"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := fingerprint(t, pool)
	var history int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM record_history`).Scan(&history); err != nil {
		t.Fatal(err)
	}

	keys := keyring(t, 1)
	var buf bytes.Buffer
	sum, err := snapshot.Create(ctx, pool, &buf, snapshot.Options{Keys: keys, Instance: "Production", Binary: "test", AttachmentsDir: files})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Rows["tasks"] != 2 || sum.AttachmentFiles != 1 || len(sum.Apps) == 0 {
		t.Fatalf("summary: rows %v, files %d, apps %v", sum.Rows["tasks"], sum.AttachmentFiles, sum.Apps)
	}
	if bytes.Contains(buf.Bytes(), []byte("Disk full")) || bytes.Contains(buf.Bytes(), []byte("pat@x.test")) {
		t.Fatal("the snapshot is readable without the key")
	}
	if _, err := snapshot.Verify(bytes.NewReader(buf.Bytes()), keys); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Change everything about it.
	if _, err := pool.Exec(ctx, `DELETE FROM attachments; DELETE FROM task_comments; DELETE FROM tasks;
		INSERT INTO users (email, name, role) VALUES ('new@x.test', 'New', 'agent')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET name = 'Patricia' WHERE id = $1`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (user_id, token_hash, family_id, expires_at) SELECT id, '\x01', gen_random_uuid(), now() + interval '1 day' FROM users LIMIT 1`); err != nil {
		t.Logf("no session row made (%v); the test still checks the rest", err)
	}
	_ = os.Remove(filepath.Join(files, "attachments", "ab", "log"))
	if _, err := pool.Exec(ctx, `SELECT nextval('task_number_incident'), nextval('task_number_incident')`); err != nil {
		t.Fatal(err)
	}

	got, err := snapshot.Restore(ctx, pool, bytes.NewReader(buf.Bytes()), snapshot.RestoreOptions{Keys: keys, AttachmentsDir: files})
	if err != nil {
		t.Fatal(err)
	}
	if got.Instance != "Production" || got.Rows["task_comments"] != 1 {
		t.Errorf("restore summary: %+v", got.Header)
	}
	after := fingerprint(t, pool)
	for table, fp := range before {
		if after[table] != fp {
			t.Errorf("%s differs after the restore", table)
		}
	}
	data, err := os.ReadFile(filepath.Join(files, "attachments", "ab", "log")) //nolint:gosec // the test's own folder
	if err != nil || string(data) != "hello" {
		t.Errorf("the attachment file: %q, %v", data, err)
	}
	var sessions, historyAfter int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&sessions)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM record_history`).Scan(&historyAfter)
	if sessions != 0 || historyAfter != history {
		t.Errorf("sessions %d (want 0), history %d (want %d: loading writes none)", sessions, historyAfter, history)
	}
	// Numbering carries on from where the snapshot was, and the triggers are back on.
	var next int64
	if err := pool.QueryRow(ctx, `INSERT INTO tasks (type, title, requester_id) VALUES ('incident', 'Next', $1) RETURNING number`, user).Scan(&next); err != nil {
		t.Fatal(err)
	}
	var first int64
	_ = pool.QueryRow(ctx, `SELECT number FROM tasks WHERE id = $1`, task).Scan(&first)
	if next != first+1 {
		t.Errorf("the next incident is %d, want %d", next, first+1)
	}
	var created int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM record_history WHERE subject_id = (SELECT id FROM tasks WHERE title = 'Next')`).Scan(&created)
	if created == 0 {
		t.Error("history triggers were left off after the restore")
	}
}

// A damaged snapshot, one from another key, or one from a newer version changes nothing.
func TestABadSnapshotChangesNothing(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	seed(t, pool)
	keys := keyring(t, 1)
	var buf bytes.Buffer
	if _, err := snapshot.Create(ctx, pool, &buf, snapshot.Options{Keys: keys}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET title = 'Changed since'`); err != nil {
		t.Fatal(err)
	}
	before := fingerprint(t, pool)

	damaged := bytes.Clone(buf.Bytes())
	damaged[len(damaged)/2] ^= 0xff
	if _, err := snapshot.Verify(bytes.NewReader(damaged), keys); err == nil {
		t.Error("verify accepted a damaged snapshot")
	}
	if _, err := snapshot.Restore(ctx, pool, bytes.NewReader(damaged), snapshot.RestoreOptions{Keys: keys}); err == nil {
		t.Error("restored a damaged snapshot")
	}
	cut := buf.Bytes()[:len(buf.Bytes())*3/4]
	if _, err := snapshot.Restore(ctx, pool, bytes.NewReader(cut), snapshot.RestoreOptions{Keys: keys}); err == nil {
		t.Error("restored a snapshot cut short")
	}
	if _, err := snapshot.Restore(ctx, pool, bytes.NewReader(buf.Bytes()), snapshot.RestoreOptions{Keys: keyring(t, 2)}); err == nil ||
		!strings.Contains(err.Error(), "which this server does not have") {
		t.Errorf("another key: %v", err)
	}
	newer := bytes.Replace(buf.Bytes(), []byte(`"schema":`), []byte(`"schema":9`), 1)
	if _, err := snapshot.Restore(ctx, pool, bytes.NewReader(newer), snapshot.RestoreOptions{Keys: keys}); !errors.Is(err, snapshot.ErrNewer) {
		t.Errorf("a newer snapshot: %v", err)
	}
	if _, err := snapshot.Restore(ctx, pool, strings.NewReader("hello"), snapshot.RestoreOptions{Keys: keys}); err == nil {
		t.Error("restored something that is not a snapshot")
	}
	after := fingerprint(t, pool)
	for table, fp := range before {
		if after[table] != fp {
			t.Errorf("%s changed after a refused restore", table)
		}
	}

	// After a key rotation the old key, kept as an old key, still opens it.
	rotated := keyring(t, 3, bytes.Repeat([]byte{1}, 32))
	if _, err := snapshot.Restore(ctx, pool, bytes.NewReader(buf.Bytes()), snapshot.RestoreOptions{Keys: rotated}); err != nil {
		t.Errorf("with the key as an old key: %v", err)
	}
}

// A snapshot from an older schema is restored by migrating down to it, loading and migrating up again, and only
// when that is allowed.
func TestAnOlderSnapshotIsMigratedUp(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	latest, err := db.LatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Migrate(context.Background(), pool); err != nil {
			t.Error(err)
		}
	})
	if err := db.MigrateTo(ctx, pool, latest-1); err != nil {
		t.Fatal(err)
	}
	seed(t, pool)
	keys := keyring(t, 1)
	var buf bytes.Buffer
	if _, err := snapshot.Create(ctx, pool, &buf, snapshot.Options{Keys: keys}); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM task_comments; DELETE FROM attachments; DELETE FROM tasks`); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Restore(ctx, pool, bytes.NewReader(buf.Bytes()), snapshot.RestoreOptions{Keys: keys}); err == nil ||
		!strings.Contains(err.Error(), "allow that") {
		t.Fatalf("restored an older snapshot without being allowed to: %v", err)
	}
	if _, err := snapshot.Restore(ctx, pool, bytes.NewReader(buf.Bytes()), snapshot.RestoreOptions{Keys: keys, ChangeSchema: true}); err != nil {
		t.Fatal(err)
	}
	if v, err := db.SchemaVersion(ctx, pool); err != nil || v != latest {
		t.Errorf("schema after: %d (%v), want %d", v, err, latest)
	}
	var tasks int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM tasks`).Scan(&tasks)
	if tasks != 2 {
		t.Errorf("%d tasks after the restore, want 2", tasks)
	}
}
