package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
)

const greet = `kind: workflow
name: greet
record_type: incident
start: hello
steps:
  hello: {type: comment.add, body: Hello, next: done}
  done: {type: end}
`

// Changes made in ServiceLater are exported as local/ files and imported elsewhere: how an edit moves from dev to
// production without moving records.
func TestLocalChangesTravelAsFiles(t *testing.T) {
	scratch(t)
	pool := dbPool(t) // before the sync: it empties the database
	if _, err := cli(t, "sync"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := apps.UpdateStudio(ctx, pool, apps.StudioUpdate{App: "incident-management",
		Files: map[string][]byte{"workflows/greet.yaml": []byte(greet)}}); err != nil {
		t.Fatal(err)
	}
	has := func() bool {
		var n int
		_ = db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM app_definitions WHERE current AND kind = 'workflow' AND name = 'greet'").Scan(&n)
		})
		return n == 1
	}
	if !has() {
		t.Fatal("the edit is installed")
	}

	dir := t.TempDir()
	if out, err := cli(t, "export-local", dir); err != nil || !strings.Contains(out, "exported 1 file") {
		t.Fatalf("export: %v\n%s", err, out)
	}
	exported, err := os.ReadFile(filepath.Join(dir, "incident-management", "workflows", "greet.yaml")) //nolint:gosec // the test's own temp dir
	if err != nil || string(exported) != greet {
		t.Fatalf("exported file: %v %q", err, exported)
	}

	// Another environment: the change is not there until the files are imported.
	if _, _, err := apps.UpdateStudio(ctx, pool, apps.StudioUpdate{App: "incident-management", ReplaceAll: true}); err != nil {
		t.Fatal(err)
	}
	if has() {
		t.Fatal("cleared")
	}
	if out, err := cli(t, "import-local", "-dry-run", dir); err != nil || !strings.Contains(out, "dry run") || has() {
		t.Fatalf("a dry run changes nothing: %v\n%s", err, out)
	}
	if out, err := cli(t, "import-local", dir); err != nil || !has() {
		t.Fatalf("import: %v\n%s", err, out)
	}

	// One app's files on their own, as an app's local/ folder holds them.
	appDir := t.TempDir()
	if _, err := cli(t, "export-local", "-app", "incident-management", appDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "workflows", "greet.yaml")); err != nil {
		t.Fatalf("-app writes the app's files straight into the folder: %v", err)
	}

	// -replace makes the instance match the folder: an empty one clears every change.
	if out, err := cli(t, "import-local", "-replace", t.TempDir()); err != nil || has() {
		t.Fatalf("replace with nothing: %v\n%s", err, out)
	}

	// Anything that is not a definition file is refused, and nothing is imported.
	bad := t.TempDir()
	_ = os.MkdirAll(filepath.Join(bad, "incident-management", "workflows"), 0o750)
	_ = os.WriteFile(filepath.Join(bad, "incident-management", "workflows", "greet.yaml"), []byte(greet), 0o600)
	_ = os.WriteFile(filepath.Join(bad, "incident-management", "notes.txt"), []byte("hi"), 0o600)
	if out, err := cli(t, "import-local", bad); err == nil || !strings.Contains(err.Error(), "notes.txt") || has() {
		t.Fatalf("a stray file: %v\n%s", err, out)
	}
	// A change that does not validate leaves the app as it was.
	broken := t.TempDir()
	_ = os.MkdirAll(filepath.Join(broken, "incident-management", "workflows"), 0o750)
	_ = os.WriteFile(filepath.Join(broken, "incident-management", "workflows", "greet.yaml"), []byte(strings.Replace(greet, "next: done", "next: nowhere", 1)), 0o600)
	if out, err := cli(t, "import-local", broken); err == nil || !strings.Contains(out, `there is no step "nowhere"`) || has() {
		t.Fatalf("a broken change: %v\n%s", err, out)
	}
}
