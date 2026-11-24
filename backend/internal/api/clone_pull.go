package api

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/backup"
	"github.com/servicelater/servicelater/backend/internal/clone"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// The scheduled pull, on the source (docs/backup-and-clone.md §2): each profile hands out pull keys, and a UAT or
// development instance holding one fetches a fresh package of that profile.

// pullKeyDTO is one of a profile's pull keys (the key itself is shown only when it is made).
type pullKeyDTO struct {
	ID      uuid.UUID `json:"id"`
	Profile string    `json:"profile"`
	Name    string    `json:"name"`
	// Prefix is the key's first characters (slp_ and eight more), to tell keys apart.
	Prefix        string     `json:"prefix"`
	CreatedByName string     `json:"created_by_name"`
	CreatedAt     time.Time  `json:"created_at"`
	LastUsedAt    *time.Time `json:"last_used_at"`
	RevokedAt     *time.Time `json:"revoked_at"`
}

func toPullKey(k dbgen.ListClonePullKeysRow) pullKeyDTO {
	out := pullKeyDTO{ID: k.ID, Profile: k.Profile, Name: k.Name, Prefix: k.Prefix, CreatedByName: k.CreatedByName, CreatedAt: k.CreatedAt.Time}
	if k.LastUsedAt.Valid {
		out.LastUsedAt = &k.LastUsedAt.Time
	}
	if k.RevokedAt.Valid {
		out.RevokedAt = &k.RevokedAt.Time
	}
	return out
}

// servedPullDTO is one pull another instance made from this one.
type servedPullDTO struct {
	ID      uuid.UUID `json:"id"`
	Profile string    `json:"profile"`
	KeyName string    `json:"key_name"`
	// Status is running, done or failed (Error says why).
	Status     string     `json:"status"`
	Error      string     `json:"error"`
	Size       int64      `json:"size"`
	Rows       int64      `json:"rows"`
	RemoteAddr string     `json:"remote_addr"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

func toServedPull(p dbgen.ClonePull) servedPullDTO {
	out := servedPullDTO{ID: p.ID, Profile: p.Profile, KeyName: p.KeyName, Status: p.Status, Error: p.Error, Size: p.Size,
		Rows: p.Rows, RemoteAddr: p.RemoteAddr, StartedAt: p.StartedAt.Time}
	if p.FinishedAt.Valid {
		out.FinishedAt = &p.FinishedAt.Time
	}
	return out
}

// pullKeyRequest names a new pull key.
type pullKeyRequest struct {
	// Name says which instance holds it (UAT).
	Name string `json:"name"`
}

// madePullKeyDTO is a new pull key, with the key itself.
type madePullKeyDTO struct {
	pullKeyDTO
	// Key is the key itself, shown only in this response: enter it on the target, in Admin → Clone in → Source.
	Key string `json:"key"`
}

// POST /clones/profiles/{name}/keys (instance:clone) with {"name"}: a pull key for this profile. Another instance
// holding it may fetch a fresh package of this profile (and nothing else) from GET /instance/clone-package.
func (h *handlers) makePullKey(w http.ResponseWriter, r *http.Request) {
	var req pullKeyRequest
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len([]rune(name)) > 100 {
		writeError(w, http.StatusUnprocessableEntity, "name the key (1 to 100 characters), such as the instance that will hold it")
		return
	}
	_, by, ok := h.whoName(w, r)
	if !ok {
		return
	}
	key, shown, hash, err := clone.NewPullKey()
	if err != nil {
		writeDBError(w, err)
		return
	}
	profile := chi.URLParam(r, "name")
	var id uuid.UUID
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		if _, err := q.GetCloneProfile(r.Context(), profile); err != nil {
			return err
		}
		id, err = q.InsertClonePullKey(r.Context(), dbgen.InsertClonePullKeyParams{Profile: profile, Name: name, Prefix: shown,
			SecretHash: hash, CreatedByName: by})
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusCreated, madePullKeyDTO{pullKeyDTO: pullKeyDTO{ID: id, Profile: profile, Name: name, Prefix: shown,
		CreatedByName: by, CreatedAt: time.Now()}, Key: key})
}

// DELETE /clones/profiles/{name}/keys/{id} (instance:clone): revoke a pull key, at once.
func (h *handlers) revokePullKey(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		n, err := q.RevokeClonePullKey(r.Context(), dbgen.RevokeClonePullKeyParams{ID: id, Profile: chi.URLParam(r, "name")})
		if err == nil && n == 0 {
			return notFound()
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pullKey reads the pull key a request came with, answering 401 when there is none that works.
func (h *handlers) pullKey(w http.ResponseWriter, r *http.Request) (dbgen.FindClonePullKeyRow, bool) {
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(key, clone.PullKeyPrefix) {
		writeError(w, http.StatusUnauthorized, "send a pull key (Admin → Clones → a profile → Pull keys) as Authorization: Bearer slp_…")
		return dbgen.FindClonePullKeyRow{}, false
	}
	k, err := h.backups.FindPullKey(r.Context(), key)
	if errors.Is(err, backup.ErrBadPullKey) {
		writeError(w, http.StatusUnauthorized, "the pull key is not valid: it may have been revoked")
		return k, false
	}
	if err != nil {
		writeDBError(w, err)
		return k, false
	}
	return k, true
}

// GET /instance/clone-source (pull key, Authorization: Bearer slp_…): this instance as a clone source: its name,
// environment and version, and the profile the key pulls. A target tests its settings with it.
func (h *handlers) getCloneSource(w http.ResponseWriter, r *http.Request) {
	k, ok := h.pullKey(w, r)
	if !ok {
		return
	}
	schema, err := db.LatestVersion()
	if err != nil {
		writeDBError(w, err)
		return
	}
	st := h.instance.State()
	writeJSON(w, http.StatusOK, clone.SourceInfo{Instance: st.Name, Environment: st.Environment, Binary: h.backups.Binary,
		Schema: schema, Profile: k.Profile})
}

// GET /instance/clone-package (pull key, Authorization: Bearer slp_…, and the passphrase to encrypt it with in the
// Clone-Passphrase header, at least 12 characters): a fresh clone package of the key's profile, made while it is
// sent. One pull of a profile at a time; every pull is recorded (Admin → Clones → Pulls).
func (h *handlers) getClonePackage(w http.ResponseWriter, r *http.Request) {
	k, ok := h.pullKey(w, r)
	if !ok {
		return
	}
	passphrase := r.Header.Get(clone.PassphraseHeader)
	if len([]rune(passphrase)) < clone.MinPassphrase {
		writeError(w, http.StatusUnprocessableEntity, "send the passphrase to encrypt the package with in the Clone-Passphrase header (at least 12 characters)")
		return
	}
	if p := r.URL.Query().Get("profile"); p != "" && p != k.Profile {
		writeError(w, http.StatusForbidden, "this key pulls the profile "+k.Profile+" only")
		return
	}
	running := false
	if err := db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM clone_pulls WHERE profile = $1 AND status = 'running'
			AND heartbeat_at > now() - interval '1 minute')`, k.Profile).Scan(&running)
	}); err != nil {
		writeDBError(w, err)
		return
	}
	if running {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "a package of "+k.Profile+" is already being sent: try again when it has finished")
		return
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remote = r.RemoteAddr
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Once the package has begun there is no answering with an error: one cut short does not open on the target.
	sent := &streamStart{w: w, start: func() { w.WriteHeader(http.StatusOK) }}
	if n, err := h.backups.ServePull(r.Context(), sent, k, passphrase, remote); err != nil && n == 0 {
		writeError(w, http.StatusInternalServerError, "the package could not be made: "+err.Error())
	}
}

// streamStart answers 200 when the first bytes are written, so an error before then can still be answered.
type streamStart struct {
	w       http.ResponseWriter
	start   func()
	started bool
}

func (s *streamStart) Write(p []byte) (int, error) {
	if !s.started {
		s.started = true
		s.start()
	}
	return s.w.Write(p)
}
