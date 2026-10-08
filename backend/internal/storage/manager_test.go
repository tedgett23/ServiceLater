package storage_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/dbtest"
	"github.com/servicelater/servicelater/backend/internal/storage"
)

// On a cluster, a server whose folder is not the shared one refuses to store files instead of keeping them where no
// other server can read them.
func TestAServerWithoutTheSharedFolderRefuses(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM settings WHERE name LIKE 'storage.%'"); err != nil {
		t.Fatal(err)
	}
	shared, private := t.TempDir(), t.TempDir()
	first := &storage.Manager{Pool: pool, LocalDir: shared}
	second := &storage.Manager{Pool: pool, LocalDir: shared} // mounts the same folder
	stray := &storage.Manager{Pool: pool, LocalDir: private} // has a folder of its own

	if _, _, err := first.Current(ctx); err != nil {
		t.Fatalf("the first server claims the folder: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(shared, storage.MarkerFile)) //nolint:gosec // a test's own temporary folder
	if err != nil || len(strings.TrimSpace(string(b))) != 36 {
		t.Fatalf("marker: %q %v", b, err)
	}
	if _, _, err := second.Current(ctx); err != nil {
		t.Errorf("a server sharing the folder: %v", err)
	}
	var fe *storage.FolderError
	if _, _, err := stray.Current(ctx); !errors.As(err, &fe) || !strings.Contains(err.Error(), private) {
		t.Fatalf("a server with its own folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(private, storage.MarkerFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the stray server marked its folder")
	}
	if _, _, err := (&storage.Manager{Pool: pool}).Current(ctx); !errors.Is(err, storage.ErrNoFolder) {
		t.Errorf("no folder at all: %v", err)
	}

	// When the old folder is gone for good, an operator claims a new one; the old one is then refused.
	if _, err := stray.ClaimFolder(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := stray.Current(ctx); err != nil {
		t.Errorf("the claimed folder: %v", err)
	}
	if _, _, err := first.Current(ctx); !errors.As(err, &fe) {
		t.Errorf("the old folder after a claim: %v", err)
	}
}
