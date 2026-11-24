package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/backup"
	"github.com/servicelater/servicelater/backend/internal/clone"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/secrets"
)

// Clone profiles and packages (docs/backup-and-clone.md §2): what a copy of this instance for UAT or development
// takes, and the encrypted packages made from them.

// cloneProfileDTO is one clone profile.
type cloneProfileDTO struct {
	// Name is short (uat, dev-small): lowercase letters, digits and dashes.
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Config      clone.Profile `json:"config"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

func toCloneProfile(p dbgen.CloneProfile) cloneProfileDTO {
	config, _ := clone.ProfileFrom(p.Config)
	return cloneProfileDTO{Name: p.Name, Description: p.Description, Config: config, UpdatedAt: p.UpdatedAt.Time}
}

// clonePackageDTO is one clone package.
type clonePackageDTO struct {
	ID      uuid.UUID     `json:"id"`
	Profile string        `json:"profile"`
	Config  clone.Profile `json:"config"`
	// Status is pending, running, ready or failed (Error says why).
	Status      string `json:"status"`
	Error       string `json:"error"`
	Destination string `json:"destination"`
	// Name is the file's name.
	Name    string     `json:"name"`
	Size    int64      `json:"size"`
	TakenAt *time.Time `json:"taken_at"`
	Rows    int64      `json:"rows"`
	// Report is what it holds and what it left out, once ready.
	Report          *clone.Report `json:"report"`
	RequestedByName string        `json:"requested_by_name"`
	CreatedAt       time.Time     `json:"created_at"`
}

func toClonePackage(p dbgen.GetClonePackageRow) clonePackageDTO {
	config, _ := clone.ProfileFrom(p.Config)
	out := clonePackageDTO{ID: p.ID, Profile: p.Profile, Config: config, Status: p.Status, Error: p.Error, Destination: p.Destination,
		Name: p.Key[strings.LastIndex(p.Key, "/")+1:], Size: p.Size, Rows: p.Rows, RequestedByName: p.RequestedByName,
		CreatedAt: p.CreatedAt.Time}
	if p.TakenAt.Valid {
		out.TakenAt = &p.TakenAt.Time
	}
	if len(p.Report) > 0 {
		var rep clone.Report
		if json.Unmarshal(p.Report, &rep) == nil {
			out.Report = &rep
		}
	}
	return out
}

// cloneTypeDTO is a record type a profile can choose.
type cloneTypeDTO struct {
	Type  string `json:"type"`
	Label string `json:"label"`
}

// clonesDTO is Admin → Clones.
type clonesDTO struct {
	Profiles []cloneProfileDTO `json:"profiles"`
	Packages []clonePackageDTO `json:"packages"`
	// Default is a new profile's settings.
	Default clone.Profile `json:"default"`
	// RecordTypes are the types a profile can take.
	RecordTypes []cloneTypeDTO `json:"record_types"`
	// MaskedFields are the form fields marked mask: true, which masking fields leaves out.
	MaskedFields []string `json:"masked_fields"`
	// KeyConfigured says whether this server has a secrets key, which holds a package's passphrase until it is made.
	KeyConfigured bool `json:"key_configured"`
	// S3Configured says whether Admin → Storage has an S3 bucket, which the s3 destination needs.
	S3Configured bool `json:"s3_configured"`
	// Folder is where packages kept in a folder are.
	Folder string `json:"folder"`
}

// GET /clones (instance:clone): the clone profiles and the packages made from them.
func (h *handlers) getClones(w http.ResponseWriter, r *http.Request) {
	out := clonesDTO{Profiles: []cloneProfileDTO{}, Packages: []clonePackageDTO{}, Default: clone.DefaultProfile,
		RecordTypes: []cloneTypeDTO{}, MaskedFields: clone.MaskedFields(h.apps), KeyConfigured: h.keys.Configured(),
		Folder: h.backups.Dir}
	if out.MaskedFields == nil {
		out.MaskedFields = []string{}
	}
	if h.files != nil {
		if st, err := h.files.Settings(r.Context()); err == nil {
			out.S3Configured = st.S3Bucket != ""
		}
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		profiles, err := q.ListCloneProfiles(r.Context())
		if err != nil {
			return err
		}
		for _, p := range profiles {
			out.Profiles = append(out.Profiles, toCloneProfile(p))
		}
		pkgs, err := q.ListClonePackages(r.Context())
		if err != nil {
			return err
		}
		for _, p := range pkgs {
			out.Packages = append(out.Packages, toClonePackage(dbgen.GetClonePackageRow(p)))
		}
		types, err := q.ListTaskTypes(r.Context())
		for _, t := range types {
			out.RecordTypes = append(out.RecordTypes, cloneTypeDTO{Type: t.Type, Label: t.Label})
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// cloneProfileRequest creates or changes a profile.
type cloneProfileRequest struct {
	// Name is given when creating one.
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Config      clone.Profile `json:"config"`
}

// checkCloneProfile says what is wrong with a profile's settings.
func (h *handlers) checkCloneProfile(p *clone.Profile) error {
	compile := clone.Compiler(h.apps)
	if err := p.Check(h.apps.Exists, func(t, when string) error { _, err := compile(t, when); return err }); err != nil {
		return invalid(err.Error())
	}
	return nil
}

// POST /clones/profiles (instance:clone) with {"name", "description", "config"}: add a clone profile. Settings left
// out of config keep their defaults (everything, as it is, nothing masked). config says
// which records come (record_types, records all|open|recent with recent_days, exclude: record_type + when), what
// comes with them (internal_notes, attachments none|kept with attachment_max_mb, history none|recent|all with
// history_days, logs), people all|referenced, passwords, and mask (names, emails, phones, text, fields).
func (h *handlers) createCloneProfile(w http.ResponseWriter, r *http.Request) {
	req := cloneProfileRequest{Config: clone.DefaultProfile} // what is left out keeps its default
	if !decode(w, r, &req) {
		return
	}
	var out dbgen.CloneProfile
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		if err := clone.CheckName(req.Name); err != nil {
			return invalid(err.Error())
		}
		if err := h.checkCloneProfile(&req.Config); err != nil {
			return err
		}
		raw, err := json.Marshal(req.Config)
		if err != nil {
			return err
		}
		out, err = q.InsertCloneProfile(r.Context(), dbgen.InsertCloneProfileParams{Name: req.Name, Description: strings.TrimSpace(req.Description), Config: raw})
		if isUniqueViolation(err) {
			return &apiError{http.StatusConflict, "there is already a profile named " + req.Name}
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusCreated, toCloneProfile(out))
}

// PUT /clones/profiles/{name} (instance:clone) with {"description", "config"}: change a clone profile. Packages
// already made keep the settings they were made with.
func (h *handlers) updateCloneProfile(w http.ResponseWriter, r *http.Request) {
	req := cloneProfileRequest{Config: clone.DefaultProfile}
	if !decode(w, r, &req) {
		return
	}
	var out dbgen.CloneProfile
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		if err := h.checkCloneProfile(&req.Config); err != nil {
			return err
		}
		raw, err := json.Marshal(req.Config)
		if err != nil {
			return err
		}
		out, err = q.UpdateCloneProfile(r.Context(), dbgen.UpdateCloneProfileParams{Name: chi.URLParam(r, "name"),
			Description: strings.TrimSpace(req.Description), Config: raw})
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, toCloneProfile(out))
}

// DELETE /clones/profiles/{name} (instance:clone): remove a clone profile. Packages made from it stay.
func (h *handlers) deleteCloneProfile(w http.ResponseWriter, r *http.Request) {
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		n, err := q.DeleteCloneProfile(r.Context(), chi.URLParam(r, "name"))
		if err == nil && n == 0 {
			return notFound()
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clonePreviewRequest is a profile's settings to preview.
type clonePreviewRequest struct {
	Config clone.Profile `json:"config"`
}

// POST /clones/preview (instance:clone) with {"config"}: what a package made with these settings would hold now
// (records kept of each type, people, attachments, links it would clear), without making one.
func (h *handlers) previewClone(w http.ResponseWriter, r *http.Request) {
	req := clonePreviewRequest{Config: clone.DefaultProfile}
	if !decode(w, r, &req) {
		return
	}
	if err := h.checkCloneProfile(&req.Config); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	rep, err := h.backups.PreviewPackage(r.Context(), req.Config)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// clonePackageRequest asks for a package.
type clonePackageRequest struct {
	Profile string `json:"profile"`
	// Passphrase encrypts the package (at least 12 characters): the target asks for it when the package is loaded.
	// It is kept, sealed under the secrets key, only until the package is made.
	Passphrase string `json:"passphrase"`
	// Destination is folder or s3 (the bucket of Admin → Storage, under clones/).
	Destination string `json:"destination"`
}

// POST /clones/packages (instance:clone) with {"profile", "passphrase", "destination"}: make a clone package of a
// profile. It is queued for the worker; GET /clones shows it, and GET /clones/packages/{id}/file downloads it.
func (h *handlers) makeClonePackage(w http.ResponseWriter, r *http.Request) {
	var req clonePackageRequest
	if !decode(w, r, &req) {
		return
	}
	var profile dbgen.CloneProfile
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		profile, err = q.GetCloneProfile(r.Context(), req.Profile)
		return err
	}) {
		return
	}
	id, name, ok := h.whoName(w, r)
	if !ok {
		return
	}
	config, err := clone.ProfileFrom(profile.Config)
	if err != nil {
		writeDBError(w, err)
		return
	}
	pkg, err := h.backups.RequestPackage(r.Context(), profile.Name, config, req.Passphrase, req.Destination, id, name)
	var bad *backup.Invalid
	switch {
	case errors.As(err, &bad):
		writeError(w, http.StatusUnprocessableEntity, bad.Error())
		return
	case errors.Is(err, secrets.ErrNoKey):
		writeError(w, http.StatusUnprocessableEntity, "the passphrase waits for the worker sealed under the secrets key, and this server has none: set SL_SECRETS_KEY (servicelater secrets gen-key)")
		return
	case err != nil:
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toClonePackage(pkg))
}

// clonePackageFor reads one package as the caller (instance:clone, RLS).
func (h *handlers) clonePackageFor(w http.ResponseWriter, r *http.Request) (dbgen.GetClonePackageRow, bool) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return dbgen.GetClonePackageRow{}, false
	}
	var p dbgen.GetClonePackageRow
	ok = h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		p, err = q.GetClonePackage(r.Context(), id)
		return err
	})
	return p, ok
}

// GET /clones/packages/{id}/file (instance:clone): download a clone package. It opens only with its passphrase.
func (h *handlers) downloadClonePackage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.clonePackageFor(w, r)
	if !ok {
		return
	}
	f, err := h.backups.OpenPackage(r.Context(), p)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+toClonePackage(p).Name+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", f.ModTime(), f)
}

// DELETE /clones/packages/{id} (instance:clone): delete a clone package and its file.
func (h *handlers) deleteClonePackage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.clonePackageFor(w, r)
	if !ok {
		return
	}
	if err := h.backups.DeletePackage(r.Context(), p); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
