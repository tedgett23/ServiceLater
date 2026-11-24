package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// installedAppDTO is an installed app.
type installedAppDTO struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Author      string `json:"author"`
	// Definitions counts what it defines; Local, how many of them carry this instance's changes (local files, or
	// edits made in the builders).
	Definitions int `json:"definitions"`
	Local       int `json:"local"`
	// Permissions are what the app's own actions may do (its async rules, workflows and run_as: app rules).
	Permissions  []string          `json:"permissions"`
	Dependencies []apps.Dependency `json:"dependencies"`
	// FirstParty apps ship with ServiceLater and are installed at every start: they are updated by upgrading
	// ServiceLater, never from here.
	FirstParty  bool      `json:"first_party"`
	InstalledBy string    `json:"installed_by"`
	InstalledAt time.Time `json:"installed_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// appPlanDTO is what installing an app does, or did.
type appPlanDTO struct {
	App string `json:"app"`
	// FromVersion is the installed version, empty for a new app.
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	// Added, Changed and Removed are its definitions, as kind and name ("workflow onboarding").
	Added   []string `json:"added"`
	Changed []string `json:"changed"`
	Removed []string `json:"removed"`
	// Renames are statuses whose records move to a new name.
	Renames []string `json:"renames"`
	// Permissions are what the app's own actions will be able to do; NewPermissions, those it does not have yet,
	// which installing approves.
	Permissions    []string `json:"permissions"`
	NewPermissions []string `json:"new_permissions"`
	// RunsAsApp are the rules that act as the app rather than as the person saving.
	RunsAsApp []string `json:"runs_as_app"`
	Notes     []string `json:"notes"`
	// NoChange is set when installing would leave everything as it is.
	NoChange bool `json:"no_change"`
}

func toAppPlan(p *apps.Plan) appPlanDTO {
	refs := func(list []apps.DefRef) []string {
		out := make([]string, len(list))
		for i, r := range list {
			out[i] = r.String()
		}
		return out
	}
	out := appPlanDTO{App: p.App, FromVersion: p.FromVersion, ToVersion: p.ToVersion, Added: refs(p.Added), Changed: refs(p.Changed),
		Removed: refs(p.Removed), Renames: []string{}, Permissions: p.Permissions, NewPermissions: p.NewPermissions,
		RunsAsApp: p.RunsAsApp, Notes: p.Notes, NoChange: p.NoChange()}
	for _, r := range p.Renames {
		out.Renames = append(out.Renames, r.RecordType+" status "+r.From+" becomes "+r.To)
	}
	for _, l := range []*[]string{&out.Permissions, &out.NewPermissions, &out.RunsAsApp, &out.Notes} {
		if *l == nil {
			*l = []string{}
		}
	}
	return out
}

// firstParty reports whether an app ships with this binary.
func firstParty(name string) bool {
	names, err := apps.BundledNames()
	return err == nil && slices.Contains(names, name)
}

// GET /apps (apps:manage): the installed apps, by name.
func (h *handlers) listInstalledApps(w http.ResponseWriter, r *http.Request) {
	list, err := apps.List(r.Context(), h.pool)
	if err != nil {
		writeDBError(w, err)
		return
	}
	out := make([]installedAppDTO, 0, len(list))
	for _, a := range list {
		d := installedAppDTO{Name: a.Name, Version: a.Version, Description: a.Description, Author: a.Author, Definitions: a.Definitions,
			Local: a.Local, Permissions: a.Permissions, Dependencies: a.Dependencies, FirstParty: firstParty(a.Name),
			InstalledBy: a.InstalledBy, InstalledAt: a.InstalledAt, UpdatedAt: a.UpdatedAt}
		if d.Permissions == nil {
			d.Permissions = []string{}
		}
		if d.Dependencies == nil {
			d.Dependencies = []apps.Dependency{}
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// uploadedApp reads a .swapp from the request body and compiles it with this instance's layers (local files and the
// builders' edits), as the command line does. First-party apps are refused: they come with ServiceLater.
func (h *handlers) uploadedApp(r *http.Request, w http.ResponseWriter) (*apps.Compiled, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, apps.MaxArchiveBytes+1))
	if err != nil {
		return nil, invalid("the app is larger than 16 MB")
	}
	if len(data) == 0 {
		return nil, invalid("send the .swapp file as the request body (servicelater app pack makes one)")
	}
	b, err := apps.LoadArchive(data)
	if err != nil {
		return nil, invalid(err.Error())
	}
	if err := b.AttachInstanceLocal(apps.LocalRoot()); err != nil {
		return nil, invalid(err.Error())
	}
	if err := b.AttachStudio(r.Context(), h.pool); err != nil {
		return nil, err
	}
	c, err := b.Validate()
	if err != nil {
		return nil, problems("the app has problems", err)
	}
	if firstParty(c.Manifest.Name) {
		return nil, &apiError{http.StatusConflict, c.Manifest.Name + " comes with ServiceLater, so it is updated by upgrading ServiceLater; change it with local files or the builders"}
	}
	return c, nil
}

// writeAppError answers for an upload or install that did not happen.
func writeAppError(w http.ResponseWriter, err error) {
	var pe *problemsError
	var ae *apiError
	switch {
	case errors.As(err, &pe):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": pe.msg, "problems": pe.problems})
	case errors.As(err, &ae):
		writeDBError(w, err)
	default:
		// What the installer says against this database (a missing dependency, records it would strand, a
		// downgrade, permissions to approve) is about the app, not a fault.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the app cannot be installed", "problems": strings.Split(err.Error(), "\n")})
	}
}

// POST /apps/plan (apps:manage) with a .swapp as the body: what installing it would do (new, an update, or no
// change), checked against this instance as an install checks it, writing nothing. 422 lists every problem.
func (h *handlers) planApp(w http.ResponseWriter, r *http.Request) {
	c, err := h.uploadedApp(r, w)
	if err != nil {
		writeAppError(w, err)
		return
	}
	plan, err := apps.Install(r.Context(), h.pool, c, apps.InstallOptions{DryRun: true, NoDowngrade: true})
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAppPlan(plan))
}

// POST /apps/install?approve_permissions=&dev= (apps:manage) with a .swapp as the body: install or update it, in one
// transaction, and answer with what was done. New permissions the app asks for are refused unless
// approve_permissions=true, which also needs roles:manage (an app can be given anything). dev=true is a
// development reload (servicelater app dev): refused unless this instance is marked development. An app older than
// the installed one is refused here; the command line can roll back.
func (h *handlers) installApp(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	approve := r.URL.Query().Get("approve_permissions") == "true"
	dev := r.URL.Query().Get("dev") == "true"
	if approve && !p.Can(authz.RolesManage) {
		writeError(w, http.StatusForbidden, "approving an app's permissions needs roles:manage as well as apps:manage")
		return
	}
	if env := h.instance.State().Environment; dev && env != settings.EnvDevelopment {
		writeError(w, http.StatusConflict, "development reload only installs into an instance marked development (Admin → Instance); this one is "+envName(env)+". Pack the app and install it here from Admin → Apps instead")
		return
	}
	c, err := h.uploadedApp(r, w)
	if err != nil {
		writeAppError(w, err)
		return
	}
	who, err := h.installer(r.Context(), p, dev)
	if err != nil {
		writeDBError(w, err)
		return
	}
	plan, err := apps.Install(r.Context(), h.pool, c, apps.InstallOptions{InstalledBy: who, ApprovePermissions: approve, NoDowngrade: !dev})
	if err != nil {
		writeAppError(w, err)
		return
	}
	// Every server reloads on the install's notification; this one at once, so the answer and the next request agree.
	// An app's roles may have changed, and with them what people hold.
	h.perms.Forget()
	if err := h.apps.Load(r.Context()); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAppPlan(plan))
}

// installer is how an install records who made it: the person's email, and "(dev)" for a development reload.
func (h *handlers) installer(ctx context.Context, p auth.Principal, dev bool) (string, error) {
	var who string
	err := db.WithUser(ctx, h.pool, p.UserID, func(tx pgx.Tx) error {
		u, err := dbgen.New(tx).GetUser(ctx, p.UserID)
		who = u.Email
		if who == "" {
			who = u.Name
		}
		return err
	})
	if dev {
		who += " (dev)"
	}
	return who, err
}

func envName(env string) string {
	if env == "" {
		return "not marked"
	}
	return "marked " + env
}

// DELETE /apps/{name}?confirm=<name> (apps:manage): remove an app and everything it defines. Records are kept: their
// types retire and fall back to the built-in lifecycle. confirm must repeat the app's name. First-party apps, and
// apps others depend on, cannot be removed.
func (h *handlers) uninstallApp(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	switch {
	case !slices.ContainsFunc(h.apps.Apps(), func(a apps.Installed) bool { return a.Name == name }):
		writeError(w, http.StatusNotFound, "not found")
		return
	case firstParty(name):
		writeError(w, http.StatusConflict, name+" comes with ServiceLater and is installed again at every start, so it cannot be removed here")
		return
	case r.URL.Query().Get("confirm") != name:
		writeError(w, http.StatusUnprocessableEntity, "type the app's name ("+name+") to confirm")
		return
	}
	if _, err := apps.Uninstall(r.Context(), h.pool, name); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	h.perms.Forget() // its roles, and their grants, are gone
	if err := h.apps.Load(r.Context()); err != nil {
		writeDBError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
