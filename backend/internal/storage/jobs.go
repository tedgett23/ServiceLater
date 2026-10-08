package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/jobs"
)

// Background jobs (worker role).
const (
	// PurgeKind removes a file from storage once its row is gone. The attachments trigger queues it, so every way a
	// row goes (a person, an expired upload, the record's deletion) is covered.
	PurgeKind = "attachment.purge"
	// ExpireKind removes an upload not finished in time.
	ExpireKind = "attachment.expire"
)

// UploadTTL is how long an upload may stay unfinished.
const UploadTTL = 24 * time.Hour

type purgePayload struct {
	Backend  string `json:"backend"`
	Key      string `json:"key"`
	UploadID string `json:"upload_id"`
	Pending  bool   `json:"pending"`
}

type expirePayload struct {
	ID uuid.UUID `json:"id"`
}

// EnqueueExpire queues the removal of an upload if it is still unfinished after UploadTTL, in q's transaction.
func EnqueueExpire(ctx context.Context, q *dbgen.Queries, id uuid.UUID) error {
	_, err := jobs.Enqueue(ctx, q, ExpireKind, expirePayload{ID: id},
		jobs.EnqueueOptions{RunAt: time.Now().Add(UploadTTL), DedupeKey: "attachment-expire:" + id.String()})
	return err
}

// PurgeHandler removes a file whose row is gone. An upload never finished is dropped as well as any file, in case it
// was completed in storage but not here.
func (m *Manager) PurgeHandler() jobs.Handler {
	return func(ctx context.Context, job jobs.Job) error {
		var p purgePayload
		if err := json.Unmarshal(job.Payload, &p); err != nil || p.Key == "" {
			return jobs.Permanent(errors.New("storage: bad purge payload"))
		}
		s, err := m.For(ctx, p.Backend)
		if err != nil {
			return err
		}
		if p.Pending {
			if err := s.Abort(ctx, p.Key, p.UploadID); err != nil {
				return err
			}
		}
		return s.Delete(ctx, p.Key)
	}
}

// ExpireHandler removes an upload that is still unfinished; its row's trigger queues the purge.
func (m *Manager) ExpireHandler() jobs.Handler {
	return func(ctx context.Context, job jobs.Job) error {
		var p expirePayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return jobs.Permanent(errors.New("storage: bad expire payload"))
		}
		return db.WithSystem(ctx, m.Pool, func(tx pgx.Tx) error {
			_, err := dbgen.New(tx).ExpireAttachment(ctx, p.ID)
			return err
		})
	}
}
