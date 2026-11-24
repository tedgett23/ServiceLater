package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/backup"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/instance"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// ---- the instance ----

// GET /instance (no sign-in needed): whether the instance is in maintenance (a restore is waiting or running: every
// other request gets 503 until it ends), so any page, the sign-in page included, can say so. Someone signed in also
// learns what this instance is called and which environment it is (the web shows a banner on every page of one that
// is not production), and who started the restore.
func (h *handlers) getInstance(w http.ResponseWriter, r *http.Request) {
	_ = h.instance.Sync(r.Context()) // fresh, so a finished restore shows at once; on an error the last state stands
	st := h.instance.State()
	if !h.signedIn(r) {
		st.Instance = settings.Instance{}
		if st.Maintenance != nil {
			m := *st.Maintenance
			m.By = ""
			st.Maintenance = &m
		}
	}
	writeJSON(w, http.StatusOK, st)
}

// signedIn reports whether the request carries a valid access token (API keys are not checked here: GET /instance
// answers them as anyone else).
func (h *handlers) signedIn(r *http.Request) bool {
	c, err := r.Cookie(auth.AccessCookie)
	if err != nil || h.tokens == nil {
		return false
	}
	_, err = h.tokens.Parse(c.Value)
	return err == nil
}

// GET /settings/instance (settings:manage): the instance's name and environment.
func (h *handlers) getInstanceSettings(w http.ResponseWriter, r *http.Request) {
	var out settings.Instance
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListSettings(r.Context(), settings.InstancePrefix)
		stored := map[string][]byte{}
		for _, s := range rows {
			stored[s.Name] = s.Value
		}
		out = settings.InstanceFrom(stored)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PUT /settings/instance (settings:manage) with {"name", "environment"}: name this instance and say which
// environment it is (production, uat, development or other).
func (h *handlers) putInstanceSettings(w http.ResponseWriter, r *http.Request) {
	var req settings.Instance
	if !decode(w, r, &req) {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		if err := req.Check(); err != nil {
			return invalid(err.Error())
		}
		for name, value := range req.Rows() {
			if err := q.UpsertSetting(r.Context(), dbgen.UpsertSettingParams{Name: name, Value: value}); err != nil {
				return err
			}
		}
		return nil
	}) {
		return
	}
	_ = h.instance.Sync(r.Context())
	writeJSON(w, http.StatusOK, req)
}

// maintenance answers 503 to every API request while a restore waits or runs, but GET /instance, which says so.
func (h *handlers) maintenance(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.instance.InMaintenance() && (r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/api/v1/instance")) {
			w.Header().Set("Retry-After", "30")
			msg := "ServiceLater is restoring a snapshot: try again in a few minutes"
			if m := h.instance.State().Maintenance; m != nil && m.Operation == "clone" {
				msg = "ServiceLater is loading a clone: try again in a few minutes"
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": msg, "maintenance": true})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- backups ----

// snapshotDTO is one snapshot.
type snapshotDTO struct {
	ID uuid.UUID `json:"id"`
	// Kind is scheduled, manual or uploaded.
	Kind string `json:"kind"`
	// Status is pending, running, ready or failed (Error says why).
	Status      string `json:"status"`
	Error       string `json:"error"`
	Destination string `json:"destination"`
	// Name is the file's name.
	Name               string     `json:"name"`
	Size               int64      `json:"size"`
	IncludeAttachments bool       `json:"include_attachments"`
	TakenAt            *time.Time `json:"taken_at"`
	// SchemaVersion is the migration version it was taken at; restoring one older than this server's needs
	// change_schema.
	SchemaVersion   *int64    `json:"schema_version"`
	KeyID           string    `json:"key_id"`
	Instance        string    `json:"instance"`
	Rows            int64     `json:"rows"`
	AttachmentFiles int32     `json:"attachment_files"`
	RequestedByName string    `json:"requested_by_name"`
	CreatedAt       time.Time `json:"created_at"`
}

func toSnapshot(s dbgen.Snapshot) snapshotDTO {
	out := snapshotDTO{ID: s.ID, Kind: s.Kind, Status: s.Status, Error: s.Error, Destination: s.Destination,
		Name: s.Key[strings.LastIndex(s.Key, "/")+1:], Size: s.Size, IncludeAttachments: s.IncludeAttachments, KeyID: s.KeyID,
		Instance: s.Instance, Rows: s.Rows, AttachmentFiles: s.AttachmentFiles, RequestedByName: s.RequestedByName, CreatedAt: s.CreatedAt.Time}
	if s.TakenAt.Valid {
		out.TakenAt = &s.TakenAt.Time
	}
	if s.SchemaVersion.Valid {
		out.SchemaVersion = &s.SchemaVersion.Int64
	}
	return out
}

// restoreDTO is one restore, asked for or done.
type restoreDTO struct {
	ID              uuid.UUID  `json:"id"`
	SnapshotID      uuid.UUID  `json:"snapshot_id"`
	SnapshotTakenAt *time.Time `json:"snapshot_taken_at"`
	// Status is pending, running, done or failed (Error says why; a failed restore changed nothing).
	Status          string     `json:"status"`
	Error           string     `json:"error"`
	RequestedByName string     `json:"requested_by_name"`
	CreatedAt       time.Time  `json:"created_at"`
	FinishedAt      *time.Time `json:"finished_at"`
}

// backupsDTO is Admin → Backups.
type backupsDTO struct {
	Settings settings.Backup `json:"settings"`
	// KeyConfigured says whether this server has a secrets key: without one no snapshot can be taken or read.
	KeyConfigured bool `json:"key_configured"`
	// KeyID is the key snapshots are encrypted under now; a snapshot under another key needs that key to restore.
	KeyID string `json:"key_id"`
	// Folder is where snapshots kept in a folder are, on the worker and api servers.
	Folder string `json:"folder"`
	// S3Configured says whether Admin → Storage has an S3 bucket, which the s3 destination needs.
	S3Configured bool `json:"s3_configured"`
	// SchemaVersion is this server's, to compare a snapshot's with.
	SchemaVersion int64         `json:"schema_version"`
	Snapshots     []snapshotDTO `json:"snapshots"`
	Restores      []restoreDTO  `json:"restores"`
}

func (h *handlers) readBackups(r *http.Request, q *dbgen.Queries) (backupsDTO, error) {
	out := backupsDTO{KeyConfigured: h.keys.Configured(), Folder: h.backups.Dir, Snapshots: []snapshotDTO{}, Restores: []restoreDTO{}}
	if out.KeyConfigured {
		out.KeyID = h.keys.Current()
	}
	rows, err := q.ListSettings(r.Context(), settings.BackupPrefix)
	if err != nil {
		return out, err
	}
	stored := map[string][]byte{}
	for _, s := range rows {
		stored[s.Name] = s.Value
	}
	out.Settings = settings.BackupFrom(stored)
	if h.files != nil {
		if st, err := h.files.Settings(r.Context()); err == nil {
			out.S3Configured = st.S3Bucket != ""
		}
	}
	if out.SchemaVersion, err = db.LatestVersion(); err != nil {
		return out, err
	}
	snaps, err := q.ListSnapshots(r.Context())
	if err != nil {
		return out, err
	}
	for _, s := range snaps {
		out.Snapshots = append(out.Snapshots, toSnapshot(s))
	}
	restores, err := q.ListRestores(r.Context())
	for _, x := range restores {
		d := restoreDTO{ID: x.ID, SnapshotID: x.SnapshotID, Status: x.Status, Error: x.Error, RequestedByName: x.RequestedByName, CreatedAt: x.CreatedAt.Time}
		if x.SnapshotTakenAt.Valid {
			d.SnapshotTakenAt = &x.SnapshotTakenAt.Time
		}
		if x.FinishedAt.Valid {
			d.FinishedAt = &x.FinishedAt.Time
		}
		out.Restores = append(out.Restores, d)
	}
	return out, err
}

// GET /backups (instance:backup): the snapshot settings, the snapshots and the latest restores.
func (h *handlers) getBackups(w http.ResponseWriter, r *http.Request) {
	var out backupsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		out, err = h.readBackups(r, q)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PUT /backups/settings (instance:backup) with the snapshot settings: on a schedule or not (enabled, cron, timezone),
// where to (folder or s3), with the attachment files kept in a folder or not, and how many to keep (keep_last, plus one
// a day for keep_days days and one a week for keep_weeks weeks). Snapshots taken by hand or uploaded are kept until
// deleted.
func (h *handlers) putBackupSettings(w http.ResponseWriter, r *http.Request) {
	var req settings.Backup
	if !decode(w, r, &req) {
		return
	}
	var out backupsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		if err := req.Check(); err != nil {
			return invalid(err.Error())
		}
		if req.Enabled && !h.keys.Configured() {
			return invalid("snapshots are encrypted under the secrets key, and this server has none: set SL_SECRETS_KEY (servicelater secrets gen-key)")
		}
		for name, value := range req.Rows() {
			if err := q.UpsertSetting(r.Context(), dbgen.UpsertSettingParams{Name: name, Value: value}); err != nil {
				return err
			}
		}
		var err error
		out, err = h.readBackups(r, q)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// whoName is the signed-in person's name, for the record of who asked.
func (h *handlers) whoName(w http.ResponseWriter, r *http.Request) (uuid.UUID, string, bool) {
	var id uuid.UUID
	var name string
	ok := h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		u, err := q.GetUser(r.Context(), p.UserID)
		id, name = p.UserID, u.Name
		return err
	})
	return id, name, ok
}

// POST /backups (instance:backup): take a snapshot now. It is queued for the worker; GET /backups shows it.
func (h *handlers) takeSnapshot(w http.ResponseWriter, r *http.Request) {
	if !h.keys.Configured() {
		writeError(w, http.StatusUnprocessableEntity, "snapshots are encrypted under the secrets key, and this server has none: set SL_SECRETS_KEY (servicelater secrets gen-key)")
		return
	}
	id, name, ok := h.whoName(w, r)
	if !ok {
		return
	}
	s, err := h.backups.Request(r.Context(), id, name)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toSnapshot(s))
}

// POST /backups/upload (instance:backup) with a snapshot file as the body: keep a snapshot made elsewhere (another
// instance, or `servicelater snapshot create`), to restore it here. It must open with this server's secrets key and
// match its own totals.
func (h *handlers) uploadSnapshot(w http.ResponseWriter, r *http.Request) {
	id, name, ok := h.whoName(w, r)
	if !ok {
		return
	}
	s, err := h.backups.Upload(r.Context(), r.Body, id, name)
	var bad *backup.Invalid
	if errors.As(err, &bad) {
		writeError(w, http.StatusUnprocessableEntity, bad.Error())
		return
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toSnapshot(s))
}

// snapshotFor reads one snapshot as the caller (instance:backup, RLS).
func (h *handlers) snapshotFor(w http.ResponseWriter, r *http.Request) (dbgen.Snapshot, bool) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return dbgen.Snapshot{}, false
	}
	var s dbgen.Snapshot
	ok = h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		s, err = q.GetSnapshot(r.Context(), id)
		return err
	})
	return s, ok
}

// GET /backups/{id}/file (instance:backup): download a snapshot. It is encrypted under the secrets key it names.
func (h *handlers) downloadSnapshot(w http.ResponseWriter, r *http.Request) {
	s, ok := h.snapshotFor(w, r)
	if !ok {
		return
	}
	f, err := h.backups.Open(r.Context(), s)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	defer func() { _ = f.Close() }()
	name := toSnapshot(s).Name
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", f.ModTime(), f)
}

// DELETE /backups/{id} (instance:backup): delete a snapshot and its file.
func (h *handlers) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	s, ok := h.snapshotFor(w, r)
	if !ok {
		return
	}
	if err := h.backups.Delete(r.Context(), s); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// restoreRequest confirms a restore.
type restoreRequest struct {
	// Confirm must be the instance's name (or "restore" when it has none): a restore replaces everything.
	Confirm string `json:"confirm"`
	// ChangeSchema allows a snapshot from an older version: the database is migrated to it, loaded and migrated up.
	ChangeSchema bool `json:"change_schema"`
}

// POST /backups/{id}/restore (instance:backup) with {"confirm", "change_schema"}: replace everything in the instance
// with the snapshot. The worker does it; until it ends the instance is in maintenance (every request but GET /instance
// gets 503), and afterwards everyone signs in again. A restore that fails changes nothing.
func (h *handlers) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	var req restoreRequest
	if !decode(w, r, &req) {
		return
	}
	s, ok := h.snapshotFor(w, r)
	if !ok {
		return
	}
	if s.Status != "ready" {
		writeError(w, http.StatusConflict, "the snapshot is not ready")
		return
	}
	latest, err := db.LatestVersion()
	if err != nil {
		writeDBError(w, err)
		return
	}
	if s.SchemaVersion.Valid && s.SchemaVersion.Int64 > latest {
		writeError(w, http.StatusConflict, "the snapshot is from a newer version of ServiceLater: update this one first")
		return
	}
	if s.SchemaVersion.Valid && s.SchemaVersion.Int64 != latest && !req.ChangeSchema {
		writeError(w, http.StatusConflict, "the snapshot is from an older version: restoring it migrates the database to that version, loads it and migrates it up again (send change_schema: true)")
		return
	}
	if !h.keys.Has(s.KeyID) {
		writeError(w, http.StatusConflict, "the snapshot is encrypted under the secrets key "+s.KeyID+", which this server does not have")
		return
	}
	want := h.instance.State().Name
	if want == "" {
		want = "restore"
	}
	if strings.TrimSpace(req.Confirm) != want {
		writeError(w, http.StatusUnprocessableEntity, `to confirm, type the instance's name ("`+want+`")`)
		return
	}
	id, name, ok := h.whoName(w, r)
	if !ok {
		return
	}
	x, err := h.backups.RequestRestore(r.Context(), s.ID, req.ChangeSchema, id, name)
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "a restore is already waiting or running")
			return
		}
		writeDBError(w, err)
		return
	}
	_ = h.instance.Sync(r.Context())
	writeJSON(w, http.StatusAccepted, restoreDTO{ID: x.ID, SnapshotID: x.SnapshotID, Status: x.Status, RequestedByName: x.RequestedByName, CreatedAt: x.CreatedAt.Time})
}

// instanceWatcher is the default when the server gives none (tests): it starts empty and syncs on GET /instance.
func instanceWatcher(d Deps) *instance.Watcher {
	if d.Instance != nil {
		return d.Instance
	}
	return instance.NewWatcher(d.Pool)
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}
