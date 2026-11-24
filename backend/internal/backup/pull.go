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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/clone"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// The scheduled pull (docs/backup-and-clone.md §2). On the source, ServePull streams a fresh package of a pull key's
// profile and records the pull. On the target, a pull is a clone_loads row in status pulling: the worker fetches it
// (runPull) into the snapshots folder or S3 like an upload, encrypted with a passphrase derived from the secrets key,
// and then it waits to be previewed and applied, or, when the schedule asked for it, is applied at once.

// ---- on the source ----

// ErrBadPullKey is a pull key that does not work: unknown or revoked. Callers never learn which.
var ErrBadPullKey = errors.New("invalid pull key")

// FindPullKey looks up a live pull key, as the system, and marks it used.
func (s *Service) FindPullKey(ctx context.Context, key string) (dbgen.FindClonePullKeyRow, error) {
	var k dbgen.FindClonePullKeyRow
	err := db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		k, err = dbgen.New(tx).FindClonePullKey(ctx, clone.HashPullKey(key))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return k, ErrBadPullKey
	}
	return k, err
}

type counter struct {
	w io.Writer
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// ServePull writes a fresh package of the key's profile to w, encrypted with passphrase, and records the pull (who,
// from where, how big, and whether it finished). It returns how much it wrote: nothing, and the caller may still
// answer with the error.
func (s *Service) ServePull(ctx context.Context, w io.Writer, k dbgen.FindClonePullKeyRow, passphrase, remote string) (int64, error) {
	var id uuid.UUID
	var config clone.Profile
	if err := db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		p, err := q.GetCloneProfile(ctx, k.Profile)
		if err != nil {
			return err
		}
		if config, err = clone.ProfileFrom(p.Config); err != nil {
			return err
		}
		id, err = q.StartServedPull(ctx, dbgen.StartServedPullParams{Profile: k.Profile, KeyID: uuid.NullUUID{UUID: k.ID, Valid: true},
			KeyName: k.Name, RemoteAddr: remote})
		return err
	}); err != nil {
		return 0, err
	}
	stop := s.beat(ctx, func(q *dbgen.Queries) error { return q.HeartbeatServedPull(ctx, id) })
	cw := &counter{w: w}
	var sum *clone.Summary
	o, _, err := s.packageOptions(ctx, k.Profile, config, passphrase)
	if err == nil {
		sum, err = clone.Export(ctx, s.Pool, cw, o)
	}
	stop()
	status, msg, rows := "done", "", int64(0)
	if err != nil {
		status, msg = "failed", err.Error()
		slog.Error("a clone pull failed", "profile", k.Profile, "key", k.Name, "err", err)
	} else {
		for _, n := range sum.Rows {
			rows += n
		}
		slog.Info("served a clone pull", "profile", k.Profile, "key", k.Name, "from", remote, "bytes", cw.n)
	}
	if ferr := db.WithSystem(context.WithoutCancel(ctx), s.Pool, func(tx pgx.Tx) error {
		return dbgen.New(tx).FinishServedPull(ctx, dbgen.FinishServedPullParams{ID: id, Status: status, Error: msg, Size: cw.n, Rows: rows})
	}); ferr != nil && err == nil {
		err = ferr
	}
	return cw.n, err
}

// ---- on the target ----

// ErrPullWaiting is a pull asked for while another is being fetched.
var ErrPullWaiting = errors.New("a pull is already being fetched")

// PullSettings reads where this instance pulls from, and the pull key, as the system.
func (s *Service) PullSettings(ctx context.Context) (settings.ClonePull, string, error) {
	return s.pullSettings(ctx, true)
}

// pullSettings reads the pull settings, and the pull key when withKey (the schedule does not need it).
func (s *Service) pullSettings(ctx context.Context, withKey bool) (settings.ClonePull, string, error) {
	var c settings.ClonePull
	var key string
	err := db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		rows, err := q.GetSystemSettings(ctx, settings.ClonePullPrefix)
		if err != nil {
			return err
		}
		stored := map[string][]byte{}
		for _, r := range rows {
			stored[r.Name] = r.Value
		}
		c = settings.ClonePullFrom(stored)
		if !withKey {
			return nil
		}
		key, err = secrets.Get(ctx, q, s.Keys, settings.ClonePullKey)
		if errors.Is(err, secrets.ErrNotSet) {
			err = nil
		}
		return err
	})
	return c, key, err
}

// Source is the clone source as set in Admin → Clone in.
func (s *Service) Source(ctx context.Context) (clone.Source, error) {
	c, key, err := s.PullSettings(ctx)
	return clone.Source{URL: c.URL, Key: key, HTTP: s.HTTP}, err
}

// RequestPull asks the worker to fetch a package from the source, to preview and apply like an upload.
func (s *Service) RequestPull(ctx context.Context, by uuid.UUID, byName string) (uuid.UUID, error) {
	src, err := s.Source(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	if src.URL == "" || src.Key == "" {
		return uuid.Nil, &Invalid{errors.New("set the source's address and pull key first")}
	}
	if !s.Keys.Configured() {
		return uuid.Nil, secrets.ErrNoKey
	}
	b, _, err := s.Settings(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		id, err = dbgen.New(tx).InsertClonePull(ctx, dbgen.InsertClonePullParams{Destination: b.Destination, KeepStudio: true,
			KeepPeople: true, RequestedBy: uuid.NullUUID{UUID: by, Valid: by != uuid.Nil}, RequestedByName: byName})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrPullWaiting
	}
	return id, err
}

// queueScheduledPull asks for a pull, to be applied once fetched, when the schedule's latest due time has none yet.
func (s *Service) queueScheduledPull(ctx context.Context) error {
	c, _, err := s.pullSettings(ctx, false)
	if err != nil || !c.Enabled || c.URL == "" {
		return err
	}
	sched, loc, err := c.Schedule()
	if err != nil {
		return nil //nolint:nilerr // a schedule that cannot be read pulls nothing; the page says why
	}
	now := s.now()
	var due time.Time
	// A week back: the default schedule is weekly.
	for at := sched.Next(now.Add(-8 * 24 * time.Hour).In(loc)); !at.IsZero() && !at.After(now); at = sched.Next(at) {
		due = at
	}
	if due.IsZero() {
		return nil
	}
	b, _, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	by, _ := uuid.Parse(c.ConfirmedBy)
	return db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('servicelater:clone-pull-schedule'))"); err != nil {
			return err
		}
		q := dbgen.New(tx)
		last, err := q.LastScheduledPull(ctx)
		if err != nil {
			return err
		}
		if !due.After(last.Time) {
			return nil
		}
		// While another pull is being fetched none is added, and this due time is asked for again next round.
		_, err = q.InsertClonePull(ctx, dbgen.InsertClonePullParams{Destination: b.Destination, KeepStudio: c.KeepStudio,
			KeepPeople: c.KeepPeople, ScheduledFor: pgtype.Timestamptz{Time: due, Valid: true}, ApplyWhenPulled: true,
			RequestedBy: uuid.NullUUID{UUID: by, Valid: by != uuid.Nil}, RequestedByName: c.ConfirmedByName + " (the schedule)"})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
}

// PullPassphrase is the passphrase a pulled package is encrypted with.
func (s *Service) PullPassphrase(id uuid.UUID, keyID string) (string, error) {
	b, err := s.Keys.Derive(keyID, "clone-pull/"+id.String())
	if err != nil {
		return "", err
	}
	return clone.PullPassphrase(b), nil
}

func (s *Service) runPull(ctx context.Context) (bool, error) {
	var l dbgen.CloneLoad
	err := db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		l, err = dbgen.New(tx).ClaimClonePull(ctx)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stop := s.beat(ctx, func(q *dbgen.Queries) error { return q.HeartbeatClonePull(ctx, l.ID) })
	err = s.pull(ctx, l)
	stop()
	if err != nil {
		slog.Error("a clone pull failed", "id", l.ID, "err", err)
		return true, db.WithSystem(context.WithoutCancel(ctx), s.Pool, func(tx pgx.Tx) error {
			return dbgen.New(tx).FailClonePull(ctx, dbgen.FailClonePullParams{ID: l.ID, Error: err.Error()})
		})
	}
	return true, nil
}

func (s *Service) pull(ctx context.Context, l dbgen.CloneLoad) error {
	src, err := s.Source(ctx)
	if err != nil {
		return err
	}
	keyID := s.Keys.Current()
	passphrase, err := s.PullPassphrase(l.ID, keyID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("the snapshots folder %s: %w", s.Dir, err)
	}
	tmp := filepath.Join(s.Dir, l.ID.String()+".partial")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600) //nolint:gosec // our own folder and an id
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	_, err = src.Fetch(ctx, passphrase, f)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	var sum *clone.Summary
	if err == nil {
		// The whole package, before it is kept: one cut short or damaged on the way fails here.
		if sum, err = clone.Verify(bufio.NewReader(f), passphrase); err != nil {
			err = fmt.Errorf("the package from the source does not open: %w (when it was cut short, the source's Admin → Clones → Pulls says why)", err)
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	name := "servicelater-clone-pull-" + s.now().UTC().Format("20060102-150405") + "-" + l.ID.String()[:8] + ".slclone"
	key, size, err := s.storeAs(ctx, l.Destination, tmp, name, "clones/")
	if err != nil {
		return err
	}
	source, _ := json.Marshal(sum.Header)
	if err := db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		return dbgen.New(tx).FinishClonePull(ctx, dbgen.FinishClonePullParams{ID: l.ID, Status: "uploaded", Key: key, Size: size,
			Source: source, PullKeyID: keyID})
	}); err != nil {
		return err
	}
	slog.Info("pulled a clone package", "id", l.ID, "from", sum.Instance, "profile", sum.Profile, "bytes", size)
	if !l.ApplyWhenPulled {
		return nil
	}
	// Asked for by the schedule: applied now, as whoever turned the schedule on, unless something stops it; then it
	// waits here like an upload, saying why.
	why := s.Guard(ctx)
	latest, err := db.LatestVersion()
	if err != nil {
		return err
	}
	if why == "" && sum.Schema > latest {
		why = "the package is from a newer version of ServiceLater: update this instance first"
	}
	if why == "" {
		err = s.RequestLoad(ctx, l.ID, passphrase, l.KeepStudio, l.KeepPeople, l.RequestedBy.UUID, l.RequestedByName)
		if err == nil {
			return nil
		}
		why = "it could not be applied: " + err.Error()
		if isUnique(err) {
			why = "another clone or restore was waiting, so it was not applied"
		}
	}
	slog.Warn("a scheduled clone pull was not applied", "id", l.ID, "why", why)
	return db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE clone_loads SET error = $2 WHERE id = $1`, l.ID, "Not applied: "+why)
		return err
	})
}

// Guard says why this instance refuses clones, if it does: one marked production does, unless an admin allowed it, so
// a wrong address can never clone development over production.
func (s *Service) Guard(ctx context.Context) string {
	_, in, err := s.Settings(ctx)
	if err == nil && in.Environment != settings.EnvProduction {
		return ""
	}
	var c settings.Clone
	if err == nil {
		err = db.WithSystem(ctx, s.Pool, func(tx pgx.Tx) error {
			var err error
			c, err = settings.ReadClone(ctx, dbgen.New(tx))
			return err
		})
	}
	if err == nil && c.AllowIntoProduction {
		return ""
	}
	return "this instance is marked production (Admin → Instance), so it refuses clones; mark it UAT or development, or allow clones into production here first"
}

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
