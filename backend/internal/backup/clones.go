package backup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/clone"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
	"github.com/servicelater/servicelater/backend/internal/storage"
)

// Clone packages (docs/backup-and-clone.md §2) are made by the same loop as snapshots, kept in the same folder or
// bucket (under clones/ in S3), and downloaded the same way. The passphrase someone chose waits sealed under the
// secrets key until the worker has used it, then is forgotten.

func passphraseName(id uuid.UUID) string { return "clone-package/" + id.String() }

// RequestPackage queues a clone package of a profile, encrypted with passphrase, to a folder or S3.
func (s *Service) RequestPackage(ctx context.Context, profile string, config clone.Profile, passphrase, destination string,
	by uuid.UUID, byName string) (dbgen.GetClonePackageRow, error) {
	if len([]rune(passphrase)) < clone.MinPassphrase {
		return dbgen.GetClonePackageRow{}, &Invalid{fmt.Errorf("the passphrase must be at least %d characters", clone.MinPassphrase)}
	}
	if destination != settings.BackupFolder && destination != settings.BackupS3 {
		return dbgen.GetClonePackageRow{}, &Invalid{fmt.Errorf("destination must be %s or %s", settings.BackupFolder, settings.BackupS3)}
	}
	if !s.Keys.Configured() {
		return dbgen.GetClonePackageRow{}, secrets.ErrNoKey
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return dbgen.GetClonePackageRow{}, err
	}
	id := uuid.New()
	sealed, err := s.Keys.Seal(passphraseName(id), passphrase)
	if err != nil {
		return dbgen.GetClonePackageRow{}, err
	}
	var out dbgen.GetClonePackageRow
	err = db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		// The id is chosen here, so the sealed passphrase names its own package.
		if _, err := tx.Exec(ctx, `INSERT INTO clone_packages (id, profile, config, destination, passphrase_key_id, passphrase_nonce,
			passphrase, requested_by, requested_by_name) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			id, profile, raw, destination, sealed.KeyID, sealed.Nonce, sealed.Ciphertext,
			uuid.NullUUID{UUID: by, Valid: by != uuid.Nil}, byName); err != nil {
			return err
		}
		out, err = dbgen.New(tx).GetClonePackage(ctx, id)
		return err
	})
	return out, err
}

// PreviewPackage works out what a package of config would hold.
func (s *Service) PreviewPackage(ctx context.Context, config clone.Profile) (*clone.Report, error) {
	return clone.Preview(ctx, s.Pool, s.cloneOptions(config))
}

func (s *Service) cloneOptions(config clone.Profile) clone.Options {
	o := clone.Options{Profile: config, Binary: s.Binary}
	if s.Registry != nil {
		o.Exclusion, o.MaskedFields = clone.Compiler(s.Registry), clone.MaskedFields(s.Registry)
	}
	return o
}

func (s *Service) runPackage(ctx context.Context) (bool, error) {
	var pkg dbgen.ClonePackage
	err := db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		pkg, err = dbgen.New(tx).ClaimClonePackage(ctx)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stop := s.beat(ctx, func(q *dbgen.Queries) error { return q.HeartbeatClonePackage(ctx, pkg.ID) })
	err = s.makePackage(ctx, pkg)
	stop()
	if err != nil {
		slog.Error("a clone package failed", "id", pkg.ID, "err", err)
		return true, db.WithSystem(context.WithoutCancel(ctx), s.Pool, func(tx pgx.Tx) error {
			return dbgen.New(tx).FailClonePackage(ctx, dbgen.FailClonePackageParams{ID: pkg.ID, Error: err.Error()})
		})
	}
	return true, nil
}

func (s *Service) makePackage(ctx context.Context, pkg dbgen.ClonePackage) error {
	passphrase, err := s.Keys.Open(passphraseName(pkg.ID), secrets.Sealed{KeyID: pkg.PassphraseKeyID, Nonce: pkg.PassphraseNonce, Ciphertext: pkg.Passphrase})
	if err != nil {
		return err
	}
	config, err := clone.ProfileFrom(pkg.Config)
	if err != nil {
		return err
	}
	_, in, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("the snapshots folder %s: %w", s.Dir, err)
	}
	name := "servicelater"
	if slug := strings.Trim(unsafe.ReplaceAllString(strings.ToLower(in.Name), "-"), "-"); slug != "" {
		name += "-" + slug
	}
	name += "-" + pkg.Profile + "-" + s.now().UTC().Format("20060102-150405") + "-" + pkg.ID.String()[:8] + ".slclone"
	tmp := filepath.Join(s.Dir, pkg.ID.String()+".partial")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // our own folder and an id
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	o := s.cloneOptions(config)
	o.ProfileName, o.Passphrase, o.Instance, o.Environment = pkg.Profile, passphrase, in.Name, in.Environment
	if s.Files != nil {
		o.Files = func(ctx context.Context, backend, key string) (io.ReadCloser, int64, error) {
			store, err := s.Files.For(ctx, backend)
			if err != nil {
				return nil, 0, err
			}
			file, err := store.Open(ctx, key)
			if err != nil {
				return nil, 0, err
			}
			return file, file.Size(), nil
		}
	}
	sum, err := clone.Export(ctx, s.Pool, f, o)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	key, size, err := s.storeAs(ctx, pkg.Destination, tmp, name, "clones/")
	if err != nil {
		return err
	}
	var rows int64
	for _, n := range sum.Rows {
		rows += n
	}
	report, err := json.Marshal(sum.Report)
	if err != nil {
		return err
	}
	return db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		return dbgen.New(tx).FinishClonePackage(ctx, dbgen.FinishClonePackageParams{ID: pkg.ID, Key: key, Size: size,
			TakenAt: pgtype.Timestamptz{Time: sum.TakenAt, Valid: true}, SchemaVersion: pgtype.Int8{Int64: sum.Schema, Valid: true},
			Rows: rows, Report: report})
	})
}

// OpenPackage opens a ready package's file.
func (s *Service) OpenPackage(ctx context.Context, pkg dbgen.GetClonePackageRow) (storage.File, error) {
	if pkg.Status != "ready" {
		return nil, errors.New("the package is not ready")
	}
	return s.openFile(ctx, pkg.Destination, pkg.Key)
}

// DeletePackage removes a package's file and its record. One still being made cannot be deleted.
func (s *Service) DeletePackage(ctx context.Context, pkg dbgen.GetClonePackageRow) error {
	if pkg.Status == "running" || pkg.Status == "pending" {
		return errors.New("the package is still being made")
	}
	if pkg.Key != "" {
		if err := s.remove(ctx, pkg.Destination, pkg.Key); err != nil {
			return err
		}
	}
	return db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error { return dbgen.New(tx).DeleteClonePackage(ctx, pkg.ID) })
}

// ---- loading a package here ----

func loadPassphraseName(id uuid.UUID) string { return "clone-load/" + id.String() }

// UploadLoad keeps a clone package uploaded to load into this instance. Only its plaintext header is read here; the
// passphrase is asked for when it is previewed or applied.
func (s *Service) UploadLoad(ctx context.Context, r io.Reader, by uuid.UUID, byName string) (dbgen.GetCloneLoadRow, error) {
	b, _, err := s.Settings(ctx)
	if err != nil {
		return dbgen.GetCloneLoadRow{}, err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return dbgen.GetCloneLoadRow{}, fmt.Errorf("the snapshots folder %s: %w", s.Dir, err)
	}
	id := uuid.New()
	tmp := filepath.Join(s.Dir, id.String()+".partial")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600) //nolint:gosec // our own folder and an id
	if err != nil {
		return dbgen.GetCloneLoadRow{}, err
	}
	defer func() { _ = os.Remove(tmp) }()
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return dbgen.GetCloneLoadRow{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return dbgen.GetCloneLoadRow{}, err
	}
	head, err := clone.ReadHeader(bufio.NewReader(f))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return dbgen.GetCloneLoadRow{}, &Invalid{err}
	}
	name := "servicelater-clone-in-" + s.now().UTC().Format("20060102-150405") + "-" + id.String()[:8] + ".slclone"
	key, size, err := s.storeAs(ctx, b.Destination, tmp, name, "clones/")
	if err != nil {
		return dbgen.GetCloneLoadRow{}, err
	}
	source, _ := json.Marshal(head)
	var out dbgen.GetCloneLoadRow
	err = db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		row, err := dbgen.New(tx).InsertCloneLoad(ctx, dbgen.InsertCloneLoadParams{Destination: b.Destination, Key: key, Size: size,
			Source: source, RequestedBy: uuid.NullUUID{UUID: by, Valid: by != uuid.Nil}, RequestedByName: byName})
		out = dbgen.GetCloneLoadRow(row)
		return err
	})
	return out, err
}

// PreviewLoad works out what applying an uploaded package would do now.
func (s *Service) PreviewLoad(ctx context.Context, l dbgen.GetCloneLoadRow, o clone.LoadOptions) (*clone.LoadPreview, error) {
	f, err := s.openFile(ctx, l.Destination, l.Key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return clone.Inspect(ctx, s.Pool, f, o)
}

// RequestLoad asks the worker to apply an uploaded package. Until it ends the instance is in maintenance.
func (s *Service) RequestLoad(ctx context.Context, id uuid.UUID, passphrase string, keepStudio, keepPeople bool, by uuid.UUID, byName string) error {
	if !s.Keys.Configured() {
		return secrets.ErrNoKey
	}
	sealed, err := s.Keys.Seal(loadPassphraseName(id), passphrase)
	if err != nil {
		return err
	}
	return db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).RequestCloneLoad(ctx, dbgen.RequestCloneLoadParams{ID: id, KeepStudio: keepStudio, KeepPeople: keepPeople,
			PassphraseKeyID: sealed.KeyID, PassphraseNonce: sealed.Nonce, Passphrase: sealed.Ciphertext,
			RequestedBy: uuid.NullUUID{UUID: by, Valid: by != uuid.Nil}, RequestedByName: byName})
		return err
	})
}

func (s *Service) runLoad(ctx context.Context) (bool, error) {
	var l dbgen.CloneLoad
	err := db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		l, err = dbgen.New(tx).ClaimCloneLoad(ctx)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stop := s.beat(ctx, func(q *dbgen.Queries) error { return q.HeartbeatCloneLoad(ctx, l.ID) })
	rep, err := s.applyLoad(ctx, l)
	stop()
	status, msg := "done", ""
	if err != nil {
		status, msg = "failed", err.Error()
		slog.Error("a clone failed to load", "id", l.ID, "err", err)
	} else {
		slog.Info("loaded a clone", "id", l.ID, "by", l.RequestedByName)
	}
	var raw []byte
	if rep != nil {
		raw, _ = json.Marshal(rep)
	}
	return true, db.WithSystem(context.WithoutCancel(ctx), s.Pool, func(tx pgx.Tx) error {
		return dbgen.New(tx).FinishCloneLoad(ctx, dbgen.FinishCloneLoadParams{ID: l.ID, Status: status, Error: msg, Report: raw})
	})
}

func (s *Service) applyLoad(ctx context.Context, l dbgen.CloneLoad) (*clone.LoadReport, error) {
	passphrase, err := s.Keys.Open(loadPassphraseName(l.ID), secrets.Sealed{KeyID: l.PassphraseKeyID, Nonce: l.PassphraseNonce, Ciphertext: l.Passphrase})
	if err != nil {
		return nil, err
	}
	f, err := s.openFile(ctx, l.Destination, l.Key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	o := clone.LoadOptions{Passphrase: passphrase, KeepPeople: l.KeepPeople, KeepStudio: l.KeepStudio}
	if l.RequestedBy.Valid {
		o.Applier = l.RequestedBy.UUID
	}
	if s.Files != nil {
		// Chosen before the load starts: the storage settings are read from a table the load's transaction locks, so
		// asking for them during the load would wait on itself for ever.
		store, st, err := s.Files.Current(ctx)
		if err != nil {
			return nil, fmt.Errorf("this instance's attachment storage: %w", err)
		}
		o.PutFile = func(ctx context.Context, key string, size int64, r io.Reader) (string, error) {
			id, err := store.Begin(ctx, key, "application/octet-stream")
			if err != nil {
				return "", err
			}
			partSize, n := storage.Plan(size)
			var parts []storage.Part
			for i := 1; i <= n; i++ {
				offset := int64(i-1) * partSize
				length := min(partSize, size-offset)
				etag, err := store.PutPart(ctx, key, id, i, offset, length, io.LimitReader(r, length))
				if err != nil {
					_ = store.Abort(context.WithoutCancel(ctx), key, id)
					return "", err
				}
				parts = append(parts, storage.Part{Number: i, ETag: etag})
			}
			return st.Backend, store.Complete(ctx, key, id, parts, size)
		}
	}
	rep, err := clone.Load(ctx, s.Pool, f, o)
	if err != nil {
		return nil, err
	}
	// Committed: install the apps again over the kept studio layer, then bring the bundled apps to this binary's.
	if err := clone.ReinstallStudio(ctx, s.Pool, rep); err != nil {
		slog.Error("after a clone: reinstalling apps over the studio layer", "err", err)
	}
	if s.SyncApps != nil {
		s.SyncApps(ctx)
	}
	return rep, nil
}

// DeleteLoad removes an uploaded package's file and its record. One being applied cannot be deleted.
func (s *Service) DeleteLoad(ctx context.Context, l dbgen.GetCloneLoadRow) error {
	if l.Status == "running" || l.Status == "pending" {
		return errors.New("the package is being loaded")
	}
	if l.Key != "" {
		if err := s.remove(ctx, l.Destination, l.Key); err != nil {
			return err
		}
	}
	return db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error { return dbgen.New(tx).DeleteCloneLoad(ctx, l.ID) })
}

// CheckLoadPassphrase says whether the passphrase opens an uploaded package, reading only its header.
func (s *Service) CheckLoadPassphrase(ctx context.Context, l dbgen.GetCloneLoadRow, passphrase string) (clone.Header, error) {
	f, err := s.openFile(ctx, l.Destination, l.Key)
	if err != nil {
		return clone.Header{}, err
	}
	defer func() { _ = f.Close() }()
	return clone.CheckPassphrase(f, passphrase)
}
