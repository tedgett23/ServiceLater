package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/backup"
	"github.com/servicelater/servicelater/backend/internal/clone"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// Clone in (docs/backup-and-clone.md §2, "Applying a clone"): loading a package made on production into this
// instance, which replaces its records and keeps what it keeps of its own.

// cloneLoadDTO is one package uploaded here, and what became of it.
type cloneLoadDTO struct {
	ID uuid.UUID `json:"id"`
	// Status is pulling (being fetched from the source), uploaded (waiting to be applied), pending, running, done or
	// failed (Error says why; a load that failed changed nothing). An uploaded one may carry an Error too: a
	// scheduled pull that was fetched but not applied says why.
	Status string `json:"status"`
	Error  string `json:"error"`
	// Origin is upload or pull (fetched from the source; it needs no passphrase).
	Origin string `json:"origin"`
	// ScheduledFor is when the schedule asked for it, for a scheduled pull.
	ScheduledFor *time.Time `json:"scheduled_for"`
	// Source is the package's header: the instance, environment and profile it came from, when, at which schema.
	Source     clone.Header `json:"source"`
	Size       int64        `json:"size"`
	KeepStudio bool         `json:"keep_studio"`
	KeepPeople bool         `json:"keep_people"`
	// Report is what the load brought and kept, once done.
	Report          *clone.LoadReport `json:"report"`
	RequestedByName string            `json:"requested_by_name"`
	CreatedAt       time.Time         `json:"created_at"`
	FinishedAt      *time.Time        `json:"finished_at"`
}

func toCloneLoad(l dbgen.GetCloneLoadRow) cloneLoadDTO {
	out := cloneLoadDTO{ID: l.ID, Status: l.Status, Error: l.Error, Origin: l.Origin, Size: l.Size, KeepStudio: l.KeepStudio,
		KeepPeople: l.KeepPeople, RequestedByName: l.RequestedByName, CreatedAt: l.CreatedAt.Time}
	if l.ScheduledFor.Valid {
		out.ScheduledFor = &l.ScheduledFor.Time
	}
	_ = json.Unmarshal(l.Source, &out.Source)
	if len(l.Report) > 0 {
		var rep clone.LoadReport
		if json.Unmarshal(l.Report, &rep) == nil {
			out.Report = &rep
		}
	}
	if l.FinishedAt.Valid {
		out.FinishedAt = &l.FinishedAt.Time
	}
	return out
}

// keptPersonDTO is someone this instance keeps when a clone replaces its people.
type keptPersonDTO struct {
	UserID  uuid.UUID `json:"user_id"`
	Name    string    `json:"name"`
	Email   string    `json:"email"`
	AddedAt time.Time `json:"added_at"`
}

// switchDTO is one thing the safety switch turns off after a clone, and whether it is on now.
type switchDTO struct {
	Label string `json:"label"`
	On    bool   `json:"on"`
	// Page is where it is turned back on (the holds are turned on here).
	Page string `json:"page"`
}

// cloneInDTO is Admin → Clone in.
type cloneInDTO struct {
	Loads []cloneLoadDTO `json:"loads"`
	// Keep are the people a clone keeps (with whoever applies it).
	Keep     []keptPersonDTO `json:"keep"`
	Settings settings.Clone  `json:"settings"`
	// Switched are the settings the last clone's safety switch turns off, as they are now.
	Switched []switchDTO `json:"switched"`
	// Environment is this instance's; a production one refuses clones unless allow_into_production.
	Environment   string `json:"environment"`
	Instance      string `json:"instance"`
	KeyConfigured bool   `json:"key_configured"`
	SchemaVersion int64  `json:"schema_version"`
	// Source is where this instance pulls packages from.
	Source cloneSourceDTO `json:"source"`
}

// cloneSourceDTO is the clone source: its address, the schedule, and whether a pull key is set (it is never shown).
type cloneSourceDTO struct {
	settings.ClonePull
	KeySet bool `json:"key_set"`
}

// GET /clone-in (instance:clone): the packages uploaded here, the people a clone keeps, and the safety switch.
func (h *handlers) getCloneIn(w http.ResponseWriter, r *http.Request) {
	out := cloneInDTO{Loads: []cloneLoadDTO{}, Keep: []keptPersonDTO{}, Switched: []switchDTO{}, KeyConfigured: h.keys.Configured()}
	st := h.instance.State()
	out.Environment, out.Instance = st.Environment, st.Name
	var err error
	if out.SchemaVersion, err = db.LatestVersion(); err != nil {
		writeDBError(w, err)
		return
	}
	// Whether the pull key is set, as the system: knowing about secrets is for those who manage settings.
	_, key, err := h.backups.PullSettings(r.Context())
	if err != nil {
		writeDBError(w, err)
		return
	}
	out.Source.KeySet = key != ""
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		loads, err := q.ListCloneLoads(r.Context())
		if err != nil {
			return err
		}
		for _, l := range loads {
			out.Loads = append(out.Loads, toCloneLoad(dbgen.GetCloneLoadRow(l)))
		}
		keep, err := q.ListCloneKeepPeople(r.Context())
		if err != nil {
			return err
		}
		for _, k := range keep {
			out.Keep = append(out.Keep, keptPersonDTO{UserID: k.UserID, Name: k.Name, Email: k.Email, AddedAt: k.AddedAt.Time})
		}
		rows, err := q.ListSettings(r.Context(), "")
		stored := map[string][]byte{}
		for _, s := range rows {
			stored[s.Name] = s.Value
		}
		out.Settings = settings.CloneFrom(stored)
		out.Source.ClonePull = settings.ClonePullFrom(stored)
		for _, s := range settings.Switched {
			out.Switched = append(out.Switched, switchDTO{Label: s.Label, On: string(stored[s.Name]) == "true", Page: s.Page})
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PUT /clone-in/settings (instance:clone) with {"allow_into_production", "hold_http", "hold_schedules",
// "hold_webhooks"}: lift (or set again) the safety switch's holds, and say whether this instance may be cloned into
// while it is marked production.
func (h *handlers) putCloneInSettings(w http.ResponseWriter, r *http.Request) {
	var req settings.Clone
	if !decode(w, r, &req) {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		for name, value := range req.Rows() {
			if err := q.UpsertSetting(r.Context(), dbgen.UpsertSettingParams{Name: name, Value: value}); err != nil {
				return err
			}
		}
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// PUT /clone-in/keep/{userId} (instance:clone): keep this person when a clone replaces this instance's people.
func (h *handlers) keepOnClone(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "userId")
	if !ok {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		return q.AddCloneKeepPerson(r.Context(), id)
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /clone-in/keep/{userId} (instance:clone): stop keeping this person on a clone.
func (h *handlers) unkeepOnClone(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "userId")
	if !ok {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		n, err := q.RemoveCloneKeepPerson(r.Context(), id)
		if err == nil && n == 0 {
			return notFound()
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /clone-in/upload (instance:clone) with a clone package as the body: keep it here to preview and apply. Only
// its header is read now; nothing changes until it is applied.
func (h *handlers) uploadCloneLoad(w http.ResponseWriter, r *http.Request) {
	id, name, ok := h.whoName(w, r)
	if !ok {
		return
	}
	l, err := h.backups.UploadLoad(r.Context(), r.Body, id, name)
	var bad *backup.Invalid
	if errors.As(err, &bad) {
		writeError(w, http.StatusUnprocessableEntity, bad.Error())
		return
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toCloneLoad(l))
}

func (h *handlers) cloneLoadFor(w http.ResponseWriter, r *http.Request) (dbgen.GetCloneLoadRow, bool) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return dbgen.GetCloneLoadRow{}, false
	}
	var l dbgen.GetCloneLoadRow
	ok = h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		l, err = q.GetCloneLoad(r.Context(), id)
		return err
	})
	return l, ok
}

// pulledPassphrase fills in the passphrase of a package pulled from the source (nobody knows it: it is derived from
// the secrets key), and refuses a pull that has nothing to apply yet.
func (h *handlers) pulledPassphrase(w http.ResponseWriter, l dbgen.GetCloneLoadRow, req *cloneLoadRequest) bool {
	if l.Origin != "pull" {
		return true
	}
	if l.Status == "pulling" || l.Key == "" {
		writeError(w, http.StatusConflict, "nothing has been fetched from the source yet")
		return false
	}
	pass, err := h.backups.PullPassphrase(l.ID, l.PullKeyID)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "the pulled package's passphrase: "+err.Error())
		return false
	}
	req.Passphrase = pass
	return true
}

// cloneLoadRequest previews or applies an uploaded package.
type cloneLoadRequest struct {
	// Passphrase is the one the package was made with (left out for a package pulled from the source).
	Passphrase string `json:"passphrase"`
	// KeepPeople keeps the people on the keep list (whoever applies it is kept either way).
	KeepPeople bool `json:"keep_people"`
	// KeepStudio keeps this instance's workflow, form and catalog edits over production's.
	KeepStudio bool `json:"keep_studio"`
	// Confirm is this instance's name (or "clone" when it has none), for applying.
	Confirm string `json:"confirm"`
}

// POST /clone-in/{id}/preview (instance:clone) with {"passphrase", "keep_people", "keep_studio"}: what applying the
// package would do now (what it brings, which of this instance's people it keeps and how, what cannot follow them,
// which studio edits are kept or set aside, and any problem that stops it), without changing anything.
func (h *handlers) previewCloneLoad(w http.ResponseWriter, r *http.Request) {
	req := cloneLoadRequest{KeepPeople: true, KeepStudio: true}
	if !decode(w, r, &req) {
		return
	}
	l, ok := h.cloneLoadFor(w, r)
	if !ok {
		return
	}
	if !h.pulledPassphrase(w, l, &req) {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	pv, err := h.backups.PreviewLoad(r.Context(), l, clone.LoadOptions{Passphrase: req.Passphrase, KeepPeople: req.KeepPeople,
		KeepStudio: req.KeepStudio, Applier: p.UserID})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if msg := h.cloneGuard(r.Context()); msg != "" {
		pv.Problems = append(pv.Problems, msg)
	}
	writeJSON(w, http.StatusOK, pv)
}

// cloneGuard says why this instance refuses clones, if it does: one marked production does, unless an admin turned
// that off, so a wrong URL can never clone development over production.
func (h *handlers) cloneGuard(ctx context.Context) string { return h.backups.Guard(ctx) }

// POST /clone-in/{id}/apply (instance:clone) with {"passphrase", "keep_people", "keep_studio", "confirm"}: replace
// this instance's records with the package's. The worker does it; until it ends the instance is in maintenance, and
// afterwards everyone signs in again. A load that fails changes nothing. An instance marked production refuses
// unless allow_into_production is set.
func (h *handlers) applyCloneLoad(w http.ResponseWriter, r *http.Request) {
	var req cloneLoadRequest
	if !decode(w, r, &req) {
		return
	}
	l, ok := h.cloneLoadFor(w, r)
	if !ok {
		return
	}
	if !h.pulledPassphrase(w, l, &req) {
		return
	}
	if msg := h.cloneGuard(r.Context()); msg != "" {
		writeError(w, http.StatusConflict, msg)
		return
	}
	want := h.instance.State().Name
	if want == "" {
		want = "clone"
	}
	if strings.TrimSpace(req.Confirm) != want {
		writeError(w, http.StatusUnprocessableEntity, `to confirm, type the instance's name ("`+want+`")`)
		return
	}
	head, err := h.backups.CheckLoadPassphrase(r.Context(), l, req.Passphrase)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	latest, err := db.LatestVersion()
	if err != nil {
		writeDBError(w, err)
		return
	}
	if head.Schema > latest {
		writeError(w, http.StatusConflict, "the package is from a newer version of ServiceLater: update this instance first")
		return
	}
	if h.instance.InMaintenance() {
		writeError(w, http.StatusConflict, "a restore or a clone is already waiting or running")
		return
	}
	id, name, ok := h.whoName(w, r)
	if !ok {
		return
	}
	err = h.backups.RequestLoad(r.Context(), l.ID, req.Passphrase, req.KeepStudio, req.KeepPeople, id, name)
	switch {
	case errors.Is(err, secrets.ErrNoKey):
		writeError(w, http.StatusUnprocessableEntity, "the passphrase waits for the worker sealed under the secrets key, and this server has none: set SL_SECRETS_KEY")
		return
	case isUniqueViolation(err):
		writeError(w, http.StatusConflict, "a clone is already waiting or running")
		return
	case err != nil:
		writeDBError(w, err)
		return
	}
	_ = h.instance.Sync(r.Context())
	w.WriteHeader(http.StatusAccepted)
}

// DELETE /clone-in/{id} (instance:clone): delete an uploaded package and its file (the record of a load goes with it).
func (h *handlers) deleteCloneLoad(w http.ResponseWriter, r *http.Request) {
	l, ok := h.cloneLoadFor(w, r)
	if !ok {
		return
	}
	if err := h.backups.DeleteLoad(r.Context(), l); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cloneSourceRequest sets where this instance pulls clones from, and when.
type cloneSourceRequest struct {
	settings.ClonePull
	// Key is the source's pull key (Admin → Clones → a profile → Pull keys there); left out or empty, the one set
	// stays. Setting it needs settings:manage too, as every secret does.
	Key string `json:"key"`
	// ClearKey forgets the pull key.
	ClearKey bool `json:"clear_key"`
	// Confirm is this instance's name (or "clone" when it has none): needed to turn the schedule on, or to change the
	// address while it is on, since every scheduled pull replaces this instance's records without asking again.
	Confirm string `json:"confirm"`
}

// PUT /clone-in/source (instance:clone) with {"url", "key", "clear_key", "enabled", "cron", "timezone",
// "keep_people", "keep_studio", "confirm"}: where this instance pulls clone packages from, and the schedule. With the
// schedule on, the worker pulls a fresh package when it is due and applies it as whoever turned the schedule on
// (confirmed_by), who is always kept. confirmed_by is set by the server.
func (h *handlers) putCloneSource(w http.ResponseWriter, r *http.Request) {
	var req cloneSourceRequest
	if !decode(w, r, &req) {
		return
	}
	if err := req.Check(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if (req.Key != "" || req.ClearKey) && !p.Can(authz.SettingsManage) {
		writeError(w, http.StatusForbidden, "the pull key is a secret, and setting one needs the settings:manage permission")
		return
	}
	if req.Key != "" && !strings.HasPrefix(strings.TrimSpace(req.Key), clone.PullKeyPrefix) {
		writeError(w, http.StatusUnprocessableEntity, "a pull key starts slp_: make one on the source, in Admin → Clones → a profile → Pull keys")
		return
	}
	if req.Key != "" && !h.keys.Configured() {
		writeError(w, http.StatusUnprocessableEntity, "the pull key is kept under the secrets key, and this server has none: set SL_SECRETS_KEY")
		return
	}
	prev, _, err := h.backups.PullSettings(r.Context())
	if err != nil {
		writeDBError(w, err)
		return
	}
	req.ConfirmedBy, req.ConfirmedByName = prev.ConfirmedBy, prev.ConfirmedByName
	if req.Enabled && (!prev.Enabled || prev.URL != req.URL) {
		if msg := h.cloneGuard(r.Context()); msg != "" {
			writeError(w, http.StatusConflict, msg)
			return
		}
		want := h.instance.State().Name
		if want == "" {
			want = "clone"
		}
		if strings.TrimSpace(req.Confirm) != want {
			writeError(w, http.StatusUnprocessableEntity, `every scheduled pull replaces this instance's records: to confirm, type the instance's name ("`+want+`")`)
			return
		}
		id, name, ok := h.whoName(w, r)
		if !ok {
			return
		}
		req.ConfirmedBy, req.ConfirmedByName = id.String(), name
	}
	if !req.Enabled {
		req.ConfirmedBy, req.ConfirmedByName = "", ""
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		for name, value := range req.Rows() {
			if err := q.UpsertSetting(r.Context(), dbgen.UpsertSettingParams{Name: name, Value: value}); err != nil {
				return err
			}
		}
		switch {
		case req.ClearKey:
			return secrets.Clear(r.Context(), q, settings.ClonePullKey)
		case req.Key != "":
			return secrets.Set(r.Context(), q, h.keys, settings.ClonePullKey, strings.TrimSpace(req.Key))
		}
		return nil
	}) {
		return
	}
	_, key, err := h.backups.PullSettings(r.Context())
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cloneSourceDTO{ClonePull: req.ClonePull, KeySet: key != ""})
}

// cloneSourceTestRequest tries a source, the one set or another.
type cloneSourceTestRequest struct {
	// URL and Key, when given, are tried instead of the ones set.
	URL string `json:"url"`
	Key string `json:"key"`
}

// cloneSourceTestDTO is what a source said about itself.
type cloneSourceTestDTO struct {
	clone.SourceInfo
	// Warnings are what may stop its packages loading here, or should make someone look twice.
	Warnings []string `json:"warnings"`
}

// POST /clone-in/source/test (instance:clone) with {"url"?, "key"?}: ask the source who it is, with the pull key,
// changing nothing: its name, environment, version and the profile the key pulls.
func (h *handlers) testCloneSource(w http.ResponseWriter, r *http.Request) {
	var req cloneSourceTestRequest
	if !decode(w, r, &req) {
		return
	}
	src, err := h.backups.Source(r.Context())
	if err != nil {
		writeDBError(w, err)
		return
	}
	if u := strings.TrimRight(strings.TrimSpace(req.URL), "/"); u != "" {
		src.URL = u
	}
	if k := strings.TrimSpace(req.Key); k != "" {
		src.Key = k
	}
	if src.URL != "" {
		if err := settings.CheckSourceURL(src.URL); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	info, err := src.Info(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := cloneSourceTestDTO{SourceInfo: info, Warnings: []string{}}
	latest, err := db.LatestVersion()
	if err != nil {
		writeDBError(w, err)
		return
	}
	if info.Schema > latest {
		out.Warnings = append(out.Warnings, "the source runs a newer version of ServiceLater: update this instance before pulling, or its packages are refused")
	}
	if st := h.instance.State(); info.Instance != "" && info.Instance == st.Name && info.Environment == st.Environment {
		out.Warnings = append(out.Warnings, "the source has this instance's name and environment: is it this instance?")
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /clone-in/pull (instance:clone): fetch a fresh package from the source now. The worker fetches it; it then
// waits under Packages to be previewed and applied like an upload, with no passphrase to type.
func (h *handlers) pullClone(w http.ResponseWriter, r *http.Request) {
	id, name, ok := h.whoName(w, r)
	if !ok {
		return
	}
	lid, err := h.backups.RequestPull(r.Context(), id, name)
	var bad *backup.Invalid
	switch {
	case errors.As(err, &bad):
		writeError(w, http.StatusUnprocessableEntity, bad.Error())
		return
	case errors.Is(err, secrets.ErrNoKey):
		writeError(w, http.StatusUnprocessableEntity, "a pulled package is encrypted under the secrets key, and this server has none: set SL_SECRETS_KEY")
		return
	case errors.Is(err, backup.ErrPullWaiting):
		writeError(w, http.StatusConflict, "a package is already being fetched from the source")
		return
	case err != nil:
		writeDBError(w, err)
		return
	}
	var l dbgen.GetCloneLoadRow
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		l, err = q.GetCloneLoad(r.Context(), lid)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusAccepted, toCloneLoad(l))
}
