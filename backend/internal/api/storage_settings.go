package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
	"github.com/servicelater/servicelater/backend/internal/storage"
)

// storageSettingsDTO is where attachments are kept. The S3 secret access key is never sent back: only whether it is
// set.
type storageSettingsDTO struct {
	settings.Storage
	// S3SecretKeySet says the secret access key is stored (encrypted); S3SecretKeyReadable is false when it was
	// stored with a key this server does not have, so it must be entered again.
	S3SecretKeySet       bool       `json:"s3_secret_key_set"`
	S3SecretKeyReadable  bool       `json:"s3_secret_key_readable"`
	S3SecretKeyUpdatedAt *time.Time `json:"s3_secret_key_updated_at"`
	// KeyConfigured says the server has a secrets key; without one the secret access key cannot be set.
	KeyConfigured bool `json:"key_configured"`
	// LocalDir is the folder this server keeps files in when they are stored locally (SL_ATTACHMENTS_DIR). It is set
	// where the server is deployed, not here; on a cluster every api and worker server must share it.
	LocalDir string `json:"local_dir"`
	// Usage is how many finished files each backend holds, and their total size in bytes. Files stay in the backend
	// they were stored in when the setting changes.
	Usage []storageUsageDTO `json:"usage"`
}

type storageUsageDTO struct {
	Backend string `json:"backend"`
	Files   int64  `json:"files"`
	Bytes   int64  `json:"bytes"`
}

// storageSettingsRequest replaces the storage settings. s3_secret_key sets the bucket's secret access key, null
// clears it, and leaving it out keeps it.
type storageSettingsRequest struct {
	settings.Storage
	S3SecretKey nullable[string] `json:"s3_secret_key"`
}

func (h *handlers) storageSettings(ctx context.Context, q *dbgen.Queries) (storageSettingsDTO, error) {
	rows, err := q.ListSettings(ctx, settings.StoragePrefix)
	if err != nil {
		return storageSettingsDTO{}, err
	}
	stored := make(map[string][]byte, len(rows))
	for _, r := range rows {
		stored[r.Name] = r.Value
	}
	out := storageSettingsDTO{Storage: settings.StorageFrom(stored), KeyConfigured: h.keys.Configured(), LocalDir: h.files.LocalDir,
		Usage: []storageUsageDTO{}}
	status, err := q.SecretStatus(ctx)
	if err != nil {
		return out, err
	}
	for _, s := range status {
		if s.Name == settings.StorageS3SecretKey {
			out.S3SecretKeySet, out.S3SecretKeyReadable = true, h.keys.Has(s.KeyID)
			at := s.UpdatedAt.Time
			out.S3SecretKeyUpdatedAt = &at
		}
	}
	// Counting every file is not a matter of RLS: ask as the system (the caller manages settings).
	err = db.WithSystem(ctx, h.pool, func(tx pgx.Tx) error {
		usage, err := dbgen.New(tx).AttachmentUsage(ctx)
		for _, u := range usage {
			out.Usage = append(out.Usage, storageUsageDTO{Backend: u.Backend, Files: u.Files, Bytes: u.Bytes})
		}
		return err
	})
	return out, err
}

// GET /settings/storage (settings:manage): where attachments are kept, without the S3 secret access key.
func (h *handlers) getStorageSettings(w http.ResponseWriter, r *http.Request) {
	var out storageSettingsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		out, err = h.storageSettings(r.Context(), q)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PUT /settings/storage (settings:manage) with every setting, and s3_secret_key to set the bucket's secret access
// key (null clears it; leave it out to keep it). New files go to the chosen backend; files already stored stay where
// they are and can still be downloaded. Choosing S3 needs an endpoint and a bucket.
func (h *handlers) putStorageSettings(w http.ResponseWriter, r *http.Request) {
	var req storageSettingsRequest
	if !decode(w, r, &req) {
		return
	}
	var out storageSettingsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		s := req.Storage
		if err := s.Check(); err != nil {
			return invalid(err.Error())
		}
		for name, value := range s.Rows() {
			if err := q.UpsertSetting(r.Context(), dbgen.UpsertSettingParams{Name: name, Value: value}); err != nil {
				return err
			}
		}
		switch {
		case !req.S3SecretKey.Set:
		case req.S3SecretKey.Value == nil || *req.S3SecretKey.Value == "":
			if err := secrets.Clear(r.Context(), q, settings.StorageS3SecretKey); err != nil {
				return err
			}
		case !h.keys.Configured():
			return invalid("the server has no secrets key, so it cannot store a secret access key: set SL_SECRETS_KEY or SL_SECRETS_KEY_FILE and restart")
		case len(*req.S3SecretKey.Value) > 1000:
			return invalid("s3_secret_key is too long")
		default:
			if err := secrets.Set(r.Context(), q, h.keys, settings.StorageS3SecretKey, *req.S3SecretKey.Value); err != nil {
				return err
			}
		}
		var err error
		out, err = h.storageSettings(r.Context(), q)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// storageTestDTO is how a storage test went: ok, or what failed. Warnings are problems that do not stop the server
// storing files but would stop browsers (an S3 bucket's CORS rules).
type storageTestDTO struct {
	OK       bool     `json:"ok"`
	Backend  string   `json:"backend"`
	Detail   string   `json:"detail,omitempty"`
	Warnings []string `json:"warnings"`
}

// POST /settings/storage/test (settings:manage): write, read back and remove a small file with the saved settings,
// as this server, and say how it went. With direct S3 uploads, it also checks that the bucket's CORS rules let
// browsers on this site upload.
func (h *handlers) testStorage(w http.ResponseWriter, r *http.Request) {
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		_, err := q.ListSettings(r.Context(), settings.StoragePrefix) // RLS: only those who manage settings get this far
		return err
	}) {
		return
	}
	out := storageTestDTO{OK: true, Warnings: []string{}}
	store, cfg, err := h.files.Current(r.Context())
	out.Backend = cfg.Backend
	if err == nil {
		err = store.Probe(r.Context())
	}
	if err != nil {
		var fe *storage.FolderError
		out.OK, out.Detail = false, err.Error()
		if !errors.As(err, &fe) && !errors.Is(err, storage.ErrNoFolder) && cfg.Backend == settings.BackendLocal {
			out.Detail = "Could not use " + h.files.LocalDir + ": " + err.Error()
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if s3, ok := store.(*storage.S3); ok && s3.Direct() {
		out.Warnings = append(out.Warnings, s3.CORSProblems(r.Context(), siteOrigin(r))...)
	}
	writeJSON(w, http.StatusOK, out)
}

// siteOrigin is the origin people open this site at, as their browser says it.
func siteOrigin(r *http.Request) string {
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
