package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/auth"
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
	// Status is uploaded (waiting to be applied), pending, running, done or failed (Error says why; a load that
	// failed changed nothing).
	Status string `json:"status"`
	Error  string `json:"error"`
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
	out := cloneLoadDTO{ID: l.ID, Status: l.Status, Error: l.Error, Size: l.Size, KeepStudio: l.KeepStudio, KeepPeople: l.KeepPeople,
		RequestedByName: l.RequestedByName, CreatedAt: l.CreatedAt.Time}
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

// cloneLoadRequest previews or applies an uploaded package.
type cloneLoadRequest struct {
	// Passphrase is the one the package was made with.
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
func (h *handlers) cloneGuard(ctx context.Context) string {
	if h.instance.State().Environment != settings.EnvProduction {
		return ""
	}
	var c settings.Clone
	err := db.WithSystem(ctx, h.pool, func(tx pgx.Tx) error {
		var err error
		c, err = settings.ReadClone(ctx, dbgen.New(tx))
		return err
	})
	if err == nil && c.AllowIntoProduction {
		return ""
	}
	return "this instance is marked production (Admin → Instance), so it refuses clones; mark it UAT or development, or allow clones into production here first"
}

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
