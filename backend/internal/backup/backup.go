// Package backup runs ServiceLater snapshots on the worker (docs/backup-and-clone.md §1): taking them on the schedule
// set in Admin → Backups or when someone asks, keeping them in a folder or the S3 bucket of Admin → Storage, pruning
// old ones, and restoring one when someone asks. The snapshots themselves are internal/snapshot; the record of them is
// the snapshots and snapshot_restores tables, which no snapshot holds and no restore empties.
//
// Snapshots and restores run in a loop of their own, not as jobs: a restore empties the job queue, and either can take
// longer than a job's lease. Each one says it is alive every 15 seconds; one silent for a minute failed.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
	"github.com/servicelater/servicelater/backend/internal/snapshot"
	"github.com/servicelater/servicelater/backend/internal/storage"
)

// Service takes, keeps and restores snapshots.
type Service struct {
	Pool *pgxpool.Pool
	Keys *secrets.Keyring
	// Files gives the S3 store of Admin → Storage, for the s3 destination.
	Files *storage.Manager
	// Dir is the snapshots folder (SL_SNAPSHOTS_DIR); files being written or uploaded wait here whatever the
	// destination.
	Dir string
	// AttachmentsDir is the attachments folder (SL_ATTACHMENTS_DIR), for snapshots that include its files.
	AttachmentsDir string
	// Binary is the running version, kept in each snapshot.
	Binary string
	// Now is the clock (tests move it).
	Now func() time.Time
	// Heartbeat is how often a running snapshot or restore says it is alive.
	Heartbeat time.Duration
	// Registry gives clone packages the forms' masked fields and compiles their exclusions.
	Registry *apps.Registry
	// SyncApps brings the bundled apps to this binary's versions, as at startup, once a clone is loaded.
	SyncApps func(context.Context)
	// HTTP fetches pulled clone packages from the source (nil: a client with no timeout; the context ends it).
	HTTP *http.Client
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) heartbeat() time.Duration {
	if s.Heartbeat > 0 {
		return s.Heartbeat
	}
	return 15 * time.Second
}

// Settings reads the backup and instance settings, as the system.
func (s *Service) Settings(ctx context.Context) (settings.Backup, settings.Instance, error) {
	var b settings.Backup
	var in settings.Instance
	err := db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		read := func(prefix string) (map[string][]byte, error) {
			rows, err := q.GetSystemSettings(ctx, prefix)
			out := map[string][]byte{}
			for _, r := range rows {
				out[r.Name] = r.Value
			}
			return out, err
		}
		stored, err := read(settings.BackupPrefix)
		if err != nil {
			return err
		}
		b = settings.BackupFrom(stored)
		if stored, err = read(settings.InstancePrefix); err != nil {
			return err
		}
		in = settings.InstanceFrom(stored)
		return nil
	})
	return b, in, err
}

// Run is the worker's loop: every interval it gives up on snapshots and restores whose worker died, queues the
// scheduled snapshot when one is due, then runs one restore or one snapshot that is waiting. Several workers may run
// it; each claim is SKIP LOCKED and the schedule is decided under an advisory lock.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Error("backups", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick does one round of Run's work.
func (s *Service) Tick(ctx context.Context) error {
	if err := db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := q.FailStaleSnapshots(ctx); err != nil {
			return err
		}
		if err := q.FailStaleRestores(ctx); err != nil {
			return err
		}
		if err := q.FailStaleClonePackages(ctx); err != nil {
			return err
		}
		if err := q.FailStaleCloneLoads(ctx); err != nil {
			return err
		}
		if err := q.FailStaleServedPulls(ctx); err != nil {
			return err
		}
		return q.FailUnclaimedRestores(ctx)
	}); err != nil {
		return err
	}
	if err := s.queueScheduled(ctx); err != nil {
		return err
	}
	// A pull schedule that cannot be queued is logged, never in the way of restores and snapshots.
	if err := s.queueScheduledPull(ctx); err != nil && ctx.Err() == nil {
		slog.Error("the clone pull schedule", "err", err)
	}
	ran, err := s.runRestore(ctx)
	if err != nil || ran {
		return err
	}
	if ran, err = s.runLoad(ctx); err != nil || ran {
		return err
	}
	if ran, err = s.runPull(ctx); err != nil || ran {
		return err
	}
	if ran, err = s.runSnapshot(ctx); err != nil || ran {
		return err
	}
	_, err = s.runPackage(ctx)
	return err
}

// ---- scheduling ----

// queueScheduled adds a scheduled snapshot when the schedule's latest due time has none yet.
func (s *Service) queueScheduled(ctx context.Context) error {
	b, _, err := s.Settings(ctx)
	if err != nil || !b.Enabled {
		return err
	}
	sched, loc, err := b.Schedule()
	if err != nil {
		return nil //nolint:nilerr // a schedule that cannot be read takes nothing; the page says why
	}
	now := s.now()
	var due time.Time
	for at := sched.Next(now.Add(-25 * time.Hour).In(loc)); !at.IsZero() && !at.After(now); at = sched.Next(at) {
		due = at
	}
	if due.IsZero() {
		return nil
	}
	return db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('servicelater:backup-schedule'))"); err != nil {
			return err
		}
		q := dbgen.New(tx)
		last, err := q.LastScheduledSnapshot(ctx)
		if err != nil {
			return err
		}
		if !due.After(last.Time) {
			return nil
		}
		_, err = q.InsertSnapshot(ctx, dbgen.InsertSnapshotParams{Kind: "scheduled", Destination: b.Destination,
			IncludeAttachments: b.Attachments, ScheduledFor: pgtype.Timestamptz{Time: due, Valid: true}, RequestedByName: "the schedule"})
		return err
	})
}

// Request queues a snapshot taken by hand, as the system (people cannot write the table).
func (s *Service) Request(ctx context.Context, by uuid.UUID, byName string) (dbgen.Snapshot, error) {
	b, _, err := s.Settings(ctx)
	if err != nil {
		return dbgen.Snapshot{}, err
	}
	var out dbgen.Snapshot
	err = db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = dbgen.New(tx).InsertSnapshot(ctx, dbgen.InsertSnapshotParams{Kind: "manual", Destination: b.Destination,
			IncludeAttachments: b.Attachments, RequestedBy: uuid.NullUUID{UUID: by, Valid: by != uuid.Nil}, RequestedByName: byName})
		return err
	})
	return out, err
}

// ---- taking one ----

func (s *Service) runSnapshot(ctx context.Context) (bool, error) {
	var snap dbgen.Snapshot
	err := db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		snap, err = dbgen.New(tx).ClaimSnapshot(ctx)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stop := s.beat(ctx, func(q *dbgen.Queries) error { return q.HeartbeatSnapshot(ctx, snap.ID) })
	err = s.take(ctx, snap)
	stop()
	if err != nil {
		slog.Error("a snapshot failed", "id", snap.ID, "err", err)
		ferr := db.WithSystem(context.WithoutCancel(ctx), s.Pool, func(tx pgx.Tx) error {
			return dbgen.New(tx).FailSnapshot(ctx, dbgen.FailSnapshotParams{ID: snap.ID, Error: err.Error()})
		})
		return true, ferr
	}
	if snap.Kind == "scheduled" {
		if err := s.Prune(ctx); err != nil {
			slog.Error("pruning snapshots", "err", err)
		}
	}
	return true, nil
}

var unsafe = regexp.MustCompile(`[^a-z0-9]+`)

func (s *Service) take(ctx context.Context, snap dbgen.Snapshot) error {
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
	name += "-" + s.now().UTC().Format("20060102-150405") + "-" + snap.ID.String()[:8] + ".slsnap"
	tmp := filepath.Join(s.Dir, snap.ID.String()+".partial")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // our own folder and an id
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	o := snapshot.Options{Keys: s.Keys, Instance: in.Name, Binary: s.Binary}
	if snap.IncludeAttachments {
		o.AttachmentsDir = s.AttachmentsDir
	}
	sum, err := snapshot.Create(ctx, s.Pool, f, o)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	key, size, err := s.store(ctx, snap.Destination, tmp, name)
	if err != nil {
		return err
	}
	var rows int64
	for _, n := range sum.Rows {
		rows += n
	}
	return db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		return dbgen.New(tx).FinishSnapshot(ctx, dbgen.FinishSnapshotParams{ID: snap.ID, Key: key, Size: size,
			TakenAt: pgtype.Timestamptz{Time: sum.TakenAt, Valid: true}, SchemaVersion: pgtype.Int8{Int64: sum.Schema, Valid: true},
			KeyID: sum.KeyID, Instance: sum.Instance, Rows: rows, AttachmentFiles: int32(sum.AttachmentFiles)}) //nolint:gosec // a count of files
	})
}

// store moves a finished file from the folder to its destination, and gives its key there and its size.
func (s *Service) store(ctx context.Context, destination, tmp, name string) (string, int64, error) {
	return s.storeAs(ctx, destination, tmp, name, "snapshots/")
}

func (s *Service) storeAs(ctx context.Context, destination, tmp, name, under string) (string, int64, error) {
	info, err := os.Stat(tmp)
	if err != nil {
		return "", 0, err
	}
	if destination == settings.BackupFolder {
		return name, info.Size(), os.Rename(tmp, filepath.Join(s.Dir, name))
	}
	store, prefix, err := s.s3(ctx)
	if err != nil {
		return "", 0, err
	}
	key := prefix + under + name
	if err := upload(ctx, store, key, tmp, info.Size()); err != nil {
		return "", 0, fmt.Errorf("uploading to S3: %w", err)
	}
	return key, info.Size(), nil
}

func (s *Service) s3(ctx context.Context) (storage.Store, string, error) {
	if s.Files == nil {
		return nil, "", errors.New("snapshots to S3 need the storage settings")
	}
	st, err := s.Files.Settings(ctx)
	if err != nil {
		return nil, "", err
	}
	if st.S3Bucket == "" {
		return nil, "", errors.New("no S3 bucket is set under Admin → Storage")
	}
	store, err := s.Files.For(ctx, settings.BackendS3)
	return store, st.S3Prefix, err
}

func upload(ctx context.Context, store storage.Store, key, path string, size int64) error {
	f, err := os.Open(path) //nolint:gosec // our own folder
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	id, err := store.Begin(ctx, key, "application/octet-stream")
	if err != nil {
		return err
	}
	partSize, n := storage.Plan(size)
	var parts []storage.Part
	for i := 1; i <= n; i++ {
		offset := int64(i-1) * partSize
		length := min(partSize, size-offset)
		etag, err := store.PutPart(ctx, key, id, i, offset, length, io.NewSectionReader(f, offset, length))
		if err != nil {
			_ = store.Abort(context.WithoutCancel(ctx), key, id)
			return err
		}
		parts = append(parts, storage.Part{Number: i, ETag: etag})
	}
	return store.Complete(ctx, key, id, parts, size)
}

// Open opens a ready snapshot's file.
func (s *Service) Open(ctx context.Context, snap dbgen.Snapshot) (storage.File, error) {
	if snap.Status != "ready" {
		return nil, errors.New("the snapshot is not ready")
	}
	return s.openFile(ctx, snap.Destination, snap.Key)
}

func (s *Service) openFile(ctx context.Context, destination, key string) (storage.File, error) {
	if destination == settings.BackupFolder {
		f, err := os.Open(filepath.Join(s.Dir, filepath.Base(key))) //nolint:gosec // a name we made, kept to the folder
		if err != nil {
			return nil, fmt.Errorf("the file is not in %s: %w", s.Dir, err)
		}
		return folderFile{f}, nil
	}
	store, _, err := s.s3(ctx)
	if err != nil {
		return nil, err
	}
	return store.Open(ctx, key)
}

type folderFile struct{ *os.File }

func (f folderFile) Size() int64 {
	info, err := f.Stat()
	if err != nil {
		return 0
	}
	return info.Size()
}

func (f folderFile) ModTime() time.Time {
	info, err := f.Stat()
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// Delete removes a snapshot's file and its record. One still being taken cannot be deleted.
func (s *Service) Delete(ctx context.Context, snap dbgen.Snapshot) error {
	if snap.Status == "running" || snap.Status == "pending" {
		return errors.New("the snapshot is still being taken")
	}
	if snap.Key != "" {
		if err := s.remove(ctx, snap.Destination, snap.Key); err != nil {
			return err
		}
	}
	return db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error { return dbgen.New(tx).DeleteSnapshot(ctx, snap.ID) })
}

func (s *Service) remove(ctx context.Context, destination, key string) error {
	if destination == settings.BackupFolder {
		err := os.Remove(filepath.Join(s.Dir, filepath.Base(key)))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	store, _, err := s.s3(ctx)
	if err != nil {
		return err
	}
	return store.Delete(ctx, key)
}

// Upload stores a snapshot made elsewhere (another instance, or the CLI), after reading it whole: it must open with
// this server's secrets key and match its own totals. It becomes a ready snapshot of kind uploaded.
func (s *Service) Upload(ctx context.Context, r io.Reader, by uuid.UUID, byName string) (dbgen.Snapshot, error) {
	b, _, err := s.Settings(ctx)
	if err != nil {
		return dbgen.Snapshot{}, err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return dbgen.Snapshot{}, fmt.Errorf("the snapshots folder %s: %w", s.Dir, err)
	}
	id := uuid.New()
	tmp := filepath.Join(s.Dir, id.String()+".partial")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600) //nolint:gosec // our own folder and an id
	if err != nil {
		return dbgen.Snapshot{}, err
	}
	defer func() { _ = os.Remove(tmp) }()
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return dbgen.Snapshot{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return dbgen.Snapshot{}, err
	}
	sum, err := snapshot.Verify(f, s.Keys)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return dbgen.Snapshot{}, &Invalid{err}
	}
	name := "servicelater-uploaded-" + s.now().UTC().Format("20060102-150405") + "-" + id.String()[:8] + ".slsnap"
	key, size, err := s.store(ctx, b.Destination, tmp, name)
	if err != nil {
		return dbgen.Snapshot{}, err
	}
	var rows int64
	for _, n := range sum.Rows {
		rows += n
	}
	var out dbgen.Snapshot
	err = db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = dbgen.New(tx).InsertUploadedSnapshot(ctx, dbgen.InsertUploadedSnapshotParams{Destination: b.Destination, Key: key,
			Size: size, IncludeAttachments: sum.Attachments, TakenAt: pgtype.Timestamptz{Time: sum.TakenAt, Valid: true},
			SchemaVersion: pgtype.Int8{Int64: sum.Schema, Valid: true}, KeyID: sum.KeyID, Instance: sum.Instance, Rows: rows,
			AttachmentFiles: int32(sum.AttachmentFiles), //nolint:gosec // a count of files
			RequestedBy:     uuid.NullUUID{UUID: by, Valid: by != uuid.Nil}, RequestedByName: byName})
		return err
	})
	return out, err
}

// Invalid is an uploaded file that is not a snapshot this server can read.
type Invalid struct{ Err error }

func (e *Invalid) Error() string { return e.Err.Error() }
func (e *Invalid) Unwrap() error { return e.Err }

// ---- retention ----

// Keep chooses which scheduled snapshots stay (newest first in taken): the newest keepLast, and the newest of each of
// the last keepDays days and keepWeeks weeks (by the calendar in loc). Everything else goes.
func Keep(taken []time.Time, keepLast, keepDays, keepWeeks int, now time.Time, loc *time.Location) map[int]bool {
	keep := map[int]bool{}
	for i := 0; i < len(taken) && i < keepLast; i++ {
		keep[i] = true
	}
	day := func(t time.Time) string { return t.In(loc).Format("2006-01-02") }
	week := func(t time.Time) string { y, w := t.In(loc).ISOWeek(); return fmt.Sprintf("%d-%02d", y, w) }
	days, weeks := map[string]bool{}, map[string]bool{}
	dayFrom := now.In(loc).AddDate(0, 0, -keepDays)
	weekFrom := now.In(loc).AddDate(0, 0, -7*keepWeeks)
	for i, t := range taken {
		if keepDays > 0 && t.After(dayFrom) && !days[day(t)] {
			days[day(t)], keep[i] = true, true
		}
		if keepWeeks > 0 && t.After(weekFrom) && !weeks[week(t)] {
			weeks[week(t)], keep[i] = true, true
		}
	}
	return keep
}

// Prune deletes the scheduled snapshots retention no longer keeps. Snapshots taken by hand or uploaded stay until
// someone deletes them.
func (s *Service) Prune(ctx context.Context) error {
	b, _, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	loc, err := time.LoadLocation(b.Timezone)
	if err != nil {
		loc = time.UTC
	}
	var rows []dbgen.ScheduledSnapshotsRow
	if err := db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		rows, err = dbgen.New(tx).ScheduledSnapshots(ctx)
		return err
	}); err != nil {
		return err
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].TakenAt.Time.After(rows[j].TakenAt.Time) })
	taken := make([]time.Time, len(rows))
	for i, r := range rows {
		taken[i] = r.TakenAt.Time
	}
	keep := Keep(taken, b.KeepLast, b.KeepDays, b.KeepWeeks, s.now(), loc)
	for i, r := range rows {
		if keep[i] {
			continue
		}
		if err := s.remove(ctx, r.Destination, r.Key); err != nil {
			return fmt.Errorf("removing %s: %w", r.Key, err)
		}
		if err := db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error { return dbgen.New(tx).DeleteSnapshot(ctx, r.ID) }); err != nil {
			return err
		}
	}
	return nil
}

// ---- restoring ----

// RequestRestore asks for a snapshot to be restored. Only one restore may wait or run at a time; while it does, the
// instance is in maintenance.
func (s *Service) RequestRestore(ctx context.Context, snapID uuid.UUID, changeSchema bool, by uuid.UUID, byName string) (dbgen.SnapshotRestore, error) {
	var out dbgen.SnapshotRestore
	err := db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = dbgen.New(tx).InsertRestore(ctx, dbgen.InsertRestoreParams{SnapshotID: snapID, ChangeSchema: changeSchema,
			RequestedBy: uuid.NullUUID{UUID: by, Valid: by != uuid.Nil}, RequestedByName: byName})
		return err
	})
	return out, err
}

func (s *Service) runRestore(ctx context.Context) (bool, error) {
	var r dbgen.SnapshotRestore
	var snap dbgen.Snapshot
	err := db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		var err error
		if r, err = q.ClaimRestore(ctx); err != nil {
			return err
		}
		snap, err = q.GetSnapshot(ctx, r.SnapshotID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stop := s.beat(ctx, func(q *dbgen.Queries) error { return q.HeartbeatRestore(ctx, r.ID) })
	err = s.restore(ctx, r, snap)
	stop()
	status, msg := "done", ""
	if err != nil {
		status, msg = "failed", err.Error()
		slog.Error("a restore failed", "id", r.ID, "err", err)
	} else {
		slog.Info("restored a snapshot", "snapshot", snap.ID, "taken", snap.TakenAt.Time, "by", r.RequestedByName)
	}
	return true, db.WithSystem(context.WithoutCancel(ctx), s.Pool, func(tx pgx.Tx) error {
		return dbgen.New(tx).FinishRestore(ctx, dbgen.FinishRestoreParams{ID: r.ID, Status: status, Error: msg})
	})
}

func (s *Service) restore(ctx context.Context, r dbgen.SnapshotRestore, snap dbgen.Snapshot) error {
	f, err := s.Open(ctx, snap)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	o := snapshot.RestoreOptions{Keys: s.Keys, ChangeSchema: r.ChangeSchema}
	if snap.IncludeAttachments {
		o.AttachmentsDir = s.AttachmentsDir
	}
	_, err = snapshot.Restore(ctx, s.Pool, f, o)
	return err
}

// beat runs fn every heartbeat interval until the returned stop is called.
func (s *Service) beat(ctx context.Context, fn func(*dbgen.Queries) error) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(s.heartbeat())
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error { return fn(dbgen.New(tx)) }); err != nil && ctx.Err() == nil {
					slog.Error("backup heartbeat", "err", err)
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}
