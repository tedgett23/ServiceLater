package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// MarkerFile is the file in the local folder that says which instance's folder it is (settings.StorageLocalID).
const MarkerFile = ".servicelater-storage"

// ErrNoFolder means this server has no attachments folder configured.
var ErrNoFolder = errors.New("this server has no attachments folder: set SL_ATTACHMENTS_DIR")

// FolderError means this server's folder is not the one the instance keeps its files in.
type FolderError struct{ Dir string }

func (e *FolderError) Error() string {
	return fmt.Sprintf("the attachments folder on this server (%s) is not the one the instance keeps its files in. On a cluster, "+
		"mount the same shared folder (NFS, EFS, CephFS, a ReadWriteMany volume) at SL_ATTACHMENTS_DIR on every api and "+
		"worker server, or store files in S3. If the files are gone for good, `servicelater storage claim-folder` makes this "+
		"folder the instance's.", e.Dir)
}

// Manager opens the instance's stores from its settings. The api and worker roles each have one; it reads the
// settings from the database on every use (they change at run time, on any server), and keeps S3 clients for reuse.
type Manager struct {
	Pool *pgxpool.Pool
	Keys *secrets.Keyring
	// LocalDir is this server's attachments folder (SL_ATTACHMENTS_DIR).
	LocalDir string

	mu sync.Mutex
	s3 map[[32]byte]*S3
}

// state is what the stores need from the database.
type state struct {
	settings.Storage
	localID   string
	secretKey string
	secretErr error
}

func (m *Manager) load(ctx context.Context) (state, error) {
	var st state
	err := db.WithSystem(ctx, m.Pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		rows, err := q.GetSystemSettings(ctx, settings.StoragePrefix)
		if err != nil {
			return err
		}
		stored := map[string][]byte{}
		for _, r := range rows {
			stored[r.Name] = r.Value
		}
		st.Storage = settings.StorageFrom(stored)
		if raw, ok := stored[settings.StorageLocalID]; ok {
			_ = json.Unmarshal(raw, &st.localID)
		}
		if st.S3AccessKeyID != "" {
			st.secretKey, st.secretErr = secrets.Get(ctx, q, m.Keys, settings.StorageS3SecretKey)
			if errors.Is(st.secretErr, secrets.ErrNotSet) {
				st.secretErr = errors.New("the S3 secret access key is not set")
			} else if st.secretErr != nil && !errors.Is(st.secretErr, secrets.ErrNoKey) {
				st.secretErr = fmt.Errorf("the S3 secret access key cannot be read: %w", st.secretErr)
			}
		}
		return nil
	})
	return st, err
}

// Settings are the instance's storage settings.
func (m *Manager) Settings(ctx context.Context) (settings.Storage, error) {
	st, err := m.load(ctx)
	return st.Storage, err
}

// Current is the store new files go to, with the settings it comes from.
func (m *Manager) Current(ctx context.Context) (Store, settings.Storage, error) {
	st, err := m.load(ctx)
	if err != nil {
		return nil, st.Storage, err
	}
	s, err := m.open(ctx, st, st.Backend)
	return s, st.Storage, err
}

// For is the store a file was kept in, whatever new files use now.
func (m *Manager) For(ctx context.Context, backend string) (Store, error) {
	st, err := m.load(ctx)
	if err != nil {
		return nil, err
	}
	return m.open(ctx, st, backend)
}

func (m *Manager) open(ctx context.Context, st state, backend string) (Store, error) {
	switch backend {
	case settings.BackendLocal:
		if err := m.checkFolder(ctx, st.localID); err != nil {
			return nil, err
		}
		return &Local{Root: m.LocalDir}, nil
	case settings.BackendS3:
		if st.secretErr != nil {
			return nil, st.secretErr
		}
		return m.s3For(st)
	}
	return nil, fmt.Errorf("storage: unknown backend %q", backend)
}

func (m *Manager) s3For(st state) (*S3, error) {
	cfg, _ := json.Marshal(st.Storage)
	k := sha256.Sum256(append(cfg, st.secretKey...))
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.s3[k]; ok {
		return s, nil
	}
	s, err := NewS3(st.Storage, st.secretKey)
	if err != nil {
		return nil, err
	}
	if m.s3 == nil || len(m.s3) > 8 {
		m.s3 = map[[32]byte]*S3{}
	}
	m.s3[k] = s
	return s, nil
}

// checkFolder makes sure this server's folder is the instance's: its marker file holds the id the database has.
// The first server to use a folder, while the database has none, claims it, writing the id to both.
func (m *Manager) checkFolder(ctx context.Context, dbID string) error {
	if m.LocalDir == "" {
		return ErrNoFolder
	}
	marker := filepath.Join(m.LocalDir, MarkerFile)
	read := func() (string, error) {
		b, err := os.ReadFile(marker) //nolint:gosec // the marker in the operator-configured folder
		return strings.TrimSpace(string(b)), err
	}
	got, err := read()
	switch {
	case err == nil && got != "" && got == dbID:
		return nil
	case err == nil && got != "" && dbID == "":
		// The folder knows its id but the database lost it (claim-folder was run elsewhere, or a restore): adopt it.
		id, cerr := m.claim(ctx, got)
		if cerr != nil {
			return cerr
		}
		if id == got {
			return nil
		}
		return &FolderError{Dir: m.LocalDir}
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("reading %s: %w", marker, err)
	case err != nil && dbID == "":
		id, err := m.claim(ctx, uuid.NewString())
		if err != nil {
			return err
		}
		if err := os.MkdirAll(m.LocalDir, 0o750); err != nil {
			return err
		}
		f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the marker in the operator-configured folder
		// Another server sharing the folder may have claimed it at the same moment.
		if errors.Is(err, fs.ErrExist) {
			if got, _ := read(); got == id {
				return nil
			}
			return &FolderError{Dir: m.LocalDir}
		}
		if err != nil {
			return err
		}
		_, werr := f.WriteString(id + "\n")
		return errors.Join(werr, f.Sync(), f.Close())
	}
	return &FolderError{Dir: m.LocalDir}
}

// claim stores id as the folder's unless one is stored already, and returns the stored one.
func (m *Manager) claim(ctx context.Context, id string) (string, error) {
	var out string
	err := db.WithSystem(ctx, m.Pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		raw, _ := json.Marshal(id)
		if err := q.ClaimStorageSetting(ctx, dbgen.ClaimStorageSettingParams{Name: settings.StorageLocalID, Value: raw}); err != nil {
			return err
		}
		v, err := q.GetSystemSetting(ctx, settings.StorageLocalID)
		if err != nil {
			return err
		}
		return json.Unmarshal(v, &out)
	})
	return out, err
}

// ClaimFolder makes this server's folder the instance's, whatever the database said before: for when the old
// folder is gone for good. Files kept in the old one can no longer be read.
func (m *Manager) ClaimFolder(ctx context.Context) (string, error) {
	if m.LocalDir == "" {
		return "", ErrNoFolder
	}
	id := uuid.NewString()
	if err := os.MkdirAll(m.LocalDir, 0o750); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(m.LocalDir, MarkerFile), []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	err := db.WithSystem(ctx, m.Pool, func(tx pgx.Tx) error {
		raw, _ := json.Marshal(id)
		q := dbgen.New(tx)
		if err := q.DeleteSystemSetting(ctx, settings.StorageLocalID); err != nil {
			return err
		}
		return q.ClaimStorageSetting(ctx, dbgen.ClaimStorageSettingParams{Name: settings.StorageLocalID, Value: raw})
	})
	return id, err
}
