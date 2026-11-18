package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/secrets"
)

// What apps' workflows may reach outside ServiceLater (docs/apps-platform.md §7): the hosts their manifests allow
// (http: permissions, approved at install) and the secrets they declare (config, type secret), whose values an admin
// sets here. A value is never shown again.

type connectionSecretDTO struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Set says a value is stored (encrypted); UpdatedAt and UpdatedBy when, and by whom.
	Set       bool       `json:"set"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	UpdatedBy string     `json:"updated_by,omitempty"`
}

type connectionDTO struct {
	App string `json:"app"`
	// Hosts are the hosts its http.request steps may call (*.example.com: its subdomains).
	Hosts   []string              `json:"hosts"`
	Secrets []connectionSecretDTO `json:"secrets"`
}

// manifests reads the installed apps' manifests, as the system (apps are not the app role's to read).
func (h *handlers) manifests(r *http.Request) (map[string]apps.Manifest, error) {
	out := map[string]apps.Manifest{}
	err := db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		for _, a := range h.apps.Apps() {
			row, err := q.GetApp(r.Context(), a.Name)
			if err != nil {
				return err
			}
			var m apps.Manifest
			if err := json.Unmarshal(row.Manifest, &m); err != nil {
				return err
			}
			out[a.Name] = m
		}
		return nil
	})
	return out, err
}

// GET /connections (settings:manage): each installed app that calls out, with the hosts it may call and the secrets
// it declares (whether each is set, never its value).
func (h *handlers) listConnections(w http.ResponseWriter, r *http.Request) {
	ms, err := h.manifests(r)
	if err != nil {
		writeDBError(w, err)
		return
	}
	out := []connectionDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		status, err := q.SecretStatus(r.Context())
		if err != nil {
			return err
		}
		for _, a := range h.apps.Apps() {
			m := ms[a.Name]
			c := connectionDTO{App: a.Name, Hosts: apps.HTTPHosts(m.Permissions), Secrets: []connectionSecretDTO{}}
			if c.Hosts == nil {
				c.Hosts = []string{}
			}
			for _, item := range m.Config {
				if item.Type != "secret" {
					continue
				}
				s := connectionSecretDTO{Key: item.Key, Label: item.Label}
				name := apps.AppSecretName(a.Name, item.Key)
				for _, st := range status {
					if st.Name == name {
						s.Set, s.UpdatedAt, s.UpdatedBy = true, &st.UpdatedAt.Time, st.UpdatedByName
					}
				}
				c.Secrets = append(c.Secrets, s)
			}
			if len(c.Hosts) > 0 || len(c.Secrets) > 0 {
				out = append(out, c)
			}
		}
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "key_configured": h.keys.Configured()})
}

// connectionSecretRequest is a secret's new value; null clears it.
type connectionSecretRequest struct {
	Value nullable[string] `json:"value"`
}

// PUT /connections/{app}/secrets/{key} (settings:manage) with {"value"}: set (or, with null, clear) a secret the app
// declares, which its http.request steps send. It is kept encrypted and never shown again.
func (h *handlers) putConnectionSecret(w http.ResponseWriter, r *http.Request) {
	app, key := chi.URLParam(r, "app"), chi.URLParam(r, "key")
	var req connectionSecretRequest
	if !decode(w, r, &req) {
		return
	}
	ms, err := h.manifests(r)
	if err != nil {
		writeDBError(w, err)
		return
	}
	m, installed := ms[app]
	if !installed || !slices.ContainsFunc(m.Config, func(c apps.ConfigItem) bool { return c.Key == key && c.Type == "secret" }) {
		writeError(w, http.StatusNotFound, "the app declares no such secret")
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		name := apps.AppSecretName(app, key)
		if !req.Value.Set || req.Value.Value == nil || *req.Value.Value == "" {
			return secrets.Clear(r.Context(), q, name)
		}
		if !h.keys.Configured() {
			return invalid("the server has no secrets key, so it cannot store the secret: set SL_SECRETS_KEY or SL_SECRETS_KEY_FILE and restart")
		}
		return secrets.Set(r.Context(), q, h.keys, name, *req.Value.Value)
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
