package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/settings"
	"github.com/servicelater/servicelater/backend/internal/storage"
)

const (
	// maxPendingUploads is how many unfinished uploads one person may have at once.
	maxPendingUploads = 20
	// maxPartURLs is how many part URLs one request may ask for.
	maxPartURLs = 100
	// partLinkTTL is how long a part's upload URL works; downloadLinkTTL a download's.
	partLinkTTL     = time.Hour
	downloadLinkTTL = 10 * time.Minute
)

// attachmentDTO is a file attached to a record.
type attachmentDTO struct {
	ID          uuid.UUID `json:"id"`
	TaskID      uuid.UUID `json:"task_id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	// Size is in bytes.
	Size int64 `json:"size"`
	// Status is pending while the upload is in flight (only its uploader sees it), then ready.
	Status        string    `json:"status"`
	CreatedBy     uuid.UUID `json:"created_by"`
	CreatedByName string    `json:"created_by_name"`
	CreatedAt     time.Time `json:"created_at"`
	// AttachedAt is when the upload finished and the file joined the record (null while it uploads): where it sits
	// among the record's comments.
	AttachedAt *time.Time `json:"attached_at"`
	// Inline says the file is a picture the browser may show (download with inline=true); anything else is saved.
	Inline bool `json:"inline"`
	// CanDelete says the reader may remove it: they added it, or may update the record.
	CanDelete bool `json:"can_delete"`
}

func toAttachment(a dbgen.Attachment, byName string, canDelete bool) attachmentDTO {
	out := attachmentDTO{ID: a.ID, TaskID: a.TaskID, Filename: a.Filename, ContentType: a.ContentType, Size: a.Size, Status: a.Status,
		CreatedBy: a.CreatedBy, CreatedByName: byName, CreatedAt: a.CreatedAt.Time, Inline: storage.Inline(a.ContentType), CanDelete: canDelete}
	if a.ReadyAt.Valid {
		at := a.ReadyAt.Time
		out.AttachedAt = &at
	}
	return out
}

func rowAttachment(a dbgen.GetAttachmentRow) dbgen.Attachment {
	return dbgen.Attachment{ID: a.ID, TaskID: a.TaskID, Filename: a.Filename, ContentType: a.ContentType, Size: a.Size, Backend: a.Backend,
		Key: a.Key, UploadID: a.UploadID, PartSize: a.PartSize, Status: a.Status, CreatedBy: a.CreatedBy, CreatedAt: a.CreatedAt, ReadyAt: a.ReadyAt}
}

func canDeleteAttachment(p auth.Principal, createdBy uuid.UUID, recordType string) bool {
	return createdBy == p.UserID || p.Perms.CanRecord(authz.Update, recordType)
}

// canAttach says whether the reader may attach files to a record: its requester, or whoever may update it.
func canAttach(p auth.Principal, t dbgen.Task) bool {
	return t.RequesterID == p.UserID || p.Perms.CanRecord(authz.Update, t.Type)
}

// attachmentsOf are a record's finished attachments.
func attachmentsOf(ctx context.Context, q *dbgen.Queries, p auth.Principal, taskID uuid.UUID, recordType string) ([]attachmentDTO, error) {
	rows, err := q.ListTaskAttachments(ctx, taskID)
	out := make([]attachmentDTO, 0, len(rows))
	for _, a := range rows {
		out = append(out, toAttachment(dbgen.Attachment{ID: a.ID, TaskID: a.TaskID, Filename: a.Filename, ContentType: a.ContentType,
			Size: a.Size, Status: a.Status, CreatedBy: a.CreatedBy, CreatedAt: a.CreatedAt, ReadyAt: a.ReadyAt}, a.CreatedByName,
			canDeleteAttachment(p, a.CreatedBy, recordType)))
	}
	return out, err
}

// storageProblem answers a store that cannot be used. Those who manage settings are told why; anyone else that
// files cannot be stored, without the server's paths.
func storageProblem(p auth.Principal, err error) error {
	slog.Error("attachment storage", "err", err)
	if p.Can(authz.SettingsManage) {
		return unavailable("Files cannot be stored: " + err.Error())
	}
	return unavailable("Files cannot be stored right now. Tell an administrator: storage is not set up correctly.")
}

// cleanFilename is the name a file is kept under: no folders, no control characters, at most 255 bytes.
func cleanFilename(name string) (string, error) {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	switch {
	case name == "" || name == "." || name == "..":
		return "", errors.New("filename is required")
	case len(name) > 255:
		return "", errors.New("filename must be at most 255 bytes")
	}
	return name, nil
}

// humanBytes writes a size the way people read it (1.5 GiB).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d bytes", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/float64(div)), "0"), ".") + " " + "KMGTPE"[exp:exp+1] + "iB"
}

// createAttachmentRequest starts uploading a file.
type createAttachmentRequest struct {
	// Filename is the file's name, as it will be shown and saved; any folder part is dropped.
	Filename string `json:"filename"`
	// Size is the file's size in bytes, at most the instance's max_size.
	Size int64 `json:"size"`
	// ContentType is the file's media type, as the browser reports it.
	ContentType string `json:"content_type"`
}

// uploadDTO is an upload in flight: the pending attachment, and how to send it. Ask for the parts' URLs with POST
// /attachments/{id}/parts, PUT each part (every part but the last is part_size bytes), then POST
// /attachments/{id}/complete with each part's ETag.
type uploadDTO struct {
	Attachment attachmentDTO `json:"attachment"`
	PartSize   int64         `json:"part_size"`
	Parts      int           `json:"parts"`
}

// POST /tickets/{id}/attachments with {filename, size, content_type} (the record's requester, or update on its
// type): start uploading a file to the record. The attachment is pending, seen only by its uploader, until the upload
// is completed; one not completed within a day is removed.
func (h *handlers) createAttachment(w http.ResponseWriter, r *http.Request) {
	var req createAttachmentRequest
	if !decode(w, r, &req) {
		return
	}
	name, err := cleanFilename(req.Filename)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if req.Size <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "the file is empty")
		return
	}
	store, cfg, serr := h.files.Current(r.Context())
	if serr == nil && req.Size > cfg.MaxSize {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("files can be at most %s", humanBytes(cfg.MaxSize)))
		return
	}
	id := uuid.New()
	prefix := ""
	if cfg.Backend == settings.BackendS3 {
		prefix = cfg.S3Prefix
	}
	key := storage.Key(prefix, id)
	partSize, parts := storage.Plan(req.Size)
	var out uploadDTO
	begun, uploadID := false, ""
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		t, err := loadTicket(r, q)
		if err != nil {
			return err
		}
		if !canAttach(p, t) {
			return forbidden(fmt.Sprintf("attaching files to it needs %s, or being its requester", authz.Record(authz.Update, t.Type)))
		}
		if serr != nil {
			return storageProblem(p, serr)
		}
		if n, err := q.CountMyPendingAttachments(r.Context()); err != nil {
			return err
		} else if n >= maxPendingUploads {
			return &apiError{http.StatusTooManyRequests, fmt.Sprintf("you have %d uploads in flight: let them finish first", n)}
		}
		if uploadID, err = store.Begin(r.Context(), key, storage.ContentType(req.ContentType)); err != nil {
			return storageProblem(p, err)
		}
		begun = true
		a, err := q.CreateAttachment(r.Context(), dbgen.CreateAttachmentParams{ID: id, TaskID: t.ID, Filename: name,
			ContentType: storage.ContentType(req.ContentType), Size: req.Size, Backend: cfg.Backend, Key: key, UploadID: uploadID, PartSize: partSize})
		if err != nil {
			return err
		}
		if err := storage.EnqueueExpire(r.Context(), q, a.ID); err != nil {
			return err
		}
		u, err := q.GetUser(r.Context(), p.UserID)
		out = uploadDTO{Attachment: toAttachment(a, u.Name, true), PartSize: partSize, Parts: parts}
		return err
	}) {
		if begun {
			if err := store.Abort(context.WithoutCancel(r.Context()), key, uploadID); err != nil {
				slog.Warn("abandoning an upload", "key", key, "err", err)
			}
		}
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// myUpload loads an upload in flight that the reader started.
func myUpload(r *http.Request, q *dbgen.Queries, p auth.Principal) (dbgen.Attachment, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return dbgen.Attachment{}, notFound()
	}
	row, err := q.GetAttachment(r.Context(), id)
	if err != nil {
		return dbgen.Attachment{}, err
	}
	if row.CreatedBy != p.UserID || row.Status != "pending" {
		return dbgen.Attachment{}, &apiError{http.StatusConflict, "that upload is already finished"}
	}
	return rowAttachment(row), nil
}

// partURLsRequest asks where to send some of an upload's parts.
type partURLsRequest struct {
	// Numbers are the parts (from 1), at most 100 at a time.
	Numbers []int `json:"numbers"`
}

// partURLDTO is where to PUT one part: the URL (absolute when it goes straight to the bucket), and how many bytes.
type partURLDTO struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Size   int64  `json:"size"`
}

// POST /attachments/{id}/parts with {numbers} (the uploader): where to PUT those parts of an upload in flight. Each
// URL works for an hour and needs no sign-in; send the part's bytes as the body, and keep the ETag response header.
func (h *handlers) attachmentPartURLs(w http.ResponseWriter, r *http.Request) {
	var req partURLsRequest
	if !decode(w, r, &req) {
		return
	}
	var a dbgen.Attachment
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		var err error
		a, err = myUpload(r, q, p)
		return err
	}) {
		return
	}
	_, parts := storage.Plan(a.Size)
	if len(req.Numbers) == 0 || len(req.Numbers) > maxPartURLs {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("ask for 1 to %d parts at a time", maxPartURLs))
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	store, err := h.files.For(r.Context(), a.Backend)
	if err != nil {
		writeDBError(w, storageProblem(p, err))
		return
	}
	direct, _ := store.(*storage.S3)
	if direct != nil && !direct.Direct() {
		direct = nil
	}
	out := make([]partURLDTO, 0, len(req.Numbers))
	for _, n := range req.Numbers {
		if n < 1 || n > parts {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("this upload has parts 1 to %d", parts))
			return
		}
		_, size := storage.PartBounds(a.Size, a.PartSize, n)
		u := fmt.Sprintf("/api/v1/attachments/%s/parts/%d?token=%s", a.ID, n,
			url.QueryEscape(h.signer.Sign(storage.ForPart, a.ID, n, time.Now().Add(partLinkTTL))))
		if direct != nil {
			if u, err = direct.PartURL(r.Context(), a.Key, a.UploadID, n, partLinkTTL); err != nil {
				writeDBError(w, storageProblem(p, err))
				return
			}
		}
		out = append(out, partURLDTO{Number: n, URL: u, Size: size})
	}
	writeJSON(w, http.StatusOK, map[string]any{"parts": out})
}

// partReceiptDTO is what storage answered for a part.
type partReceiptDTO struct {
	ETag string `json:"etag"`
}

// PUT /attachments/{id}/parts/{n}?token= with the part's bytes as the body (a URL from POST
// /attachments/{id}/parts, no sign-in): store one part of an upload in flight. Answers its ETag, also as the ETag
// header.
func (h *handlers) putAttachmentPart(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	n, nerr := strconv.Atoi(chi.URLParam(r, "n"))
	if err != nil || nerr != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if !h.signer.Verify(r.URL.Query().Get("token"), storage.ForPart, id, n, time.Now()) {
		writeError(w, http.StatusForbidden, "this upload link is not valid, or has expired")
		return
	}
	var a dbgen.Attachment
	if err := db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		a, err = dbgen.New(tx).GetAttachmentSystem(r.Context(), id)
		return err
	}); err != nil {
		writeDBError(w, err)
		return
	}
	if a.Status != "pending" {
		writeError(w, http.StatusConflict, "that upload is already finished")
		return
	}
	if _, parts := storage.Plan(a.Size); n < 1 || n > parts {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	offset, size := storage.PartBounds(a.Size, a.PartSize, n)
	if r.ContentLength >= 0 && r.ContentLength != size {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("part %d is %d bytes", n, size))
		return
	}
	store, err := h.files.For(r.Context(), a.Backend)
	if err != nil {
		writeDBError(w, storageProblem(auth.Principal{}, err))
		return
	}
	etag, err := store.PutPart(r.Context(), a.Key, a.UploadID, n, offset, size, r.Body)
	if err == nil {
		if extra, _ := r.Body.Read(make([]byte, 1)); extra > 0 {
			err = storage.ErrSize // longer than the part: a body of unknown length can only be checked at its end
		}
	}
	switch {
	case errors.Is(err, storage.ErrSize):
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("part %d is %d bytes", n, size))
		return
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, "that upload is gone: start again")
		return
	case err != nil:
		writeDBError(w, storageProblem(auth.Principal{}, err))
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, partReceiptDTO{ETag: etag})
}

// completeAttachmentRequest finishes an upload.
type completeAttachmentRequest struct {
	// Parts are every part, in order, with the ETag storage answered for it.
	Parts []storage.Part `json:"parts"`
}

// POST /attachments/{id}/complete with {parts} (the uploader): finish an upload. The file is then attached: everyone
// who sees the record sees it. A file that did not arrive whole is dropped (422): upload it again.
func (h *handlers) completeAttachment(w http.ResponseWriter, r *http.Request) {
	var req completeAttachmentRequest
	if !decode(w, r, &req) {
		return
	}
	var a dbgen.Attachment
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		var err error
		a, err = myUpload(r, q, p)
		return err
	}) {
		return
	}
	_, parts := storage.Plan(a.Size)
	if len(req.Parts) != parts {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("this upload has %d parts", parts))
		return
	}
	for i, pt := range req.Parts {
		if pt.Number != i+1 || pt.ETag == "" {
			writeError(w, http.StatusUnprocessableEntity, "parts must be listed in order, each with its ETag")
			return
		}
	}
	p, _ := auth.PrincipalFrom(r.Context())
	store, err := h.files.For(r.Context(), a.Backend)
	if err != nil {
		writeDBError(w, storageProblem(p, err))
		return
	}
	if err := store.Complete(r.Context(), a.Key, a.UploadID, req.Parts, a.Size); err != nil {
		if !errors.Is(err, storage.ErrSize) && !errors.Is(err, storage.ErrNotFound) {
			writeDBError(w, storageProblem(p, err))
			return
		}
		// Drop it: its row's trigger queues the removal of whatever arrived.
		if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
			_, err := q.DeleteAttachment(r.Context(), a.ID)
			return err
		}) {
			return
		}
		writeError(w, http.StatusUnprocessableEntity, "the file did not arrive whole: upload it again")
		return
	}
	var out attachmentDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		done, err := q.FinishAttachment(r.Context(), a.ID)
		if err != nil {
			return err
		}
		u, err := q.GetUser(r.Context(), p.UserID)
		out = toAttachment(done, u.Name, true)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// downloadDTO is where to fetch a file: a short-lived URL that needs no sign-in, to open or link to.
type downloadDTO struct {
	URL string `json:"url"`
	// ExpiresAt is when the URL stops working (a download already started carries on).
	ExpiresAt time.Time `json:"expires_at"`
}

// GET /attachments/{id}/download?inline=true (whoever sees the record): a link to the file, good for ten minutes.
// With inline=true a picture is shown rather than saved; any other file is always saved.
func (h *handlers) downloadAttachment(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	inline := r.URL.Query().Get("inline") == "true"
	var a dbgen.Attachment
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		row, err := q.GetAttachment(r.Context(), id)
		if err != nil {
			return err
		}
		if row.Status != "ready" {
			return notFound()
		}
		a = rowAttachment(row)
		return nil
	}) {
		return
	}
	inline = inline && storage.Inline(a.ContentType)
	exp := time.Now().Add(downloadLinkTTL)
	p, _ := auth.PrincipalFrom(r.Context())
	store, err := h.files.For(r.Context(), a.Backend)
	if err != nil {
		writeDBError(w, storageProblem(p, err))
		return
	}
	if s3, ok := store.(*storage.S3); ok && s3.Direct() {
		u, err := s3.DownloadURL(r.Context(), a.Key, a.Filename, a.ContentType, inline, downloadLinkTTL)
		if err != nil {
			writeDBError(w, storageProblem(p, err))
			return
		}
		writeJSON(w, http.StatusOK, downloadDTO{URL: u, ExpiresAt: exp})
		return
	}
	purpose := storage.ForDownload
	if inline {
		purpose = storage.ForInline
	}
	u := fmt.Sprintf("/api/v1/attachments/%s/file?token=%s", a.ID, url.QueryEscape(h.signer.Sign(purpose, a.ID, 0, exp)))
	writeJSON(w, http.StatusOK, downloadDTO{URL: u, ExpiresAt: exp})
}

// GET /attachments/{id}/file?token= (a URL from GET /attachments/{id}/download, no sign-in): the file's bytes. Ranges
// are supported, so a large download can resume.
func (h *handlers) serveAttachment(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	token, now := r.URL.Query().Get("token"), time.Now()
	inline := h.signer.Verify(token, storage.ForInline, id, 0, now)
	if !inline && !h.signer.Verify(token, storage.ForDownload, id, 0, now) {
		writeError(w, http.StatusForbidden, "this download link is not valid, or has expired")
		return
	}
	var a dbgen.Attachment
	if err := db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		var err error
		a, err = dbgen.New(tx).GetAttachmentSystem(r.Context(), id)
		return err
	}); err != nil {
		writeDBError(w, err)
		return
	}
	if a.Status != "ready" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	store, err := h.files.For(r.Context(), a.Backend)
	if err != nil {
		writeDBError(w, storageProblem(auth.Principal{}, err))
		return
	}
	f, err := store.Open(r.Context(), a.Key)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "the file is missing from storage")
		return
	}
	if err != nil {
		writeDBError(w, storageProblem(auth.Principal{}, err))
		return
	}
	defer func() { _ = f.Close() }()
	ct, disp := storage.Headers(a.Filename, a.ContentType, inline)
	hd := w.Header()
	hd.Set("Content-Type", ct)
	hd.Set("Content-Disposition", disp)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	hd.Set("Cache-Control", "private, max-age=600")
	if r.Method == http.MethodGet && r.Header.Get("Range") == "" && r.Header.Get("If-Modified-Since") == "" {
		hd.Set("Content-Length", strconv.FormatInt(f.Size(), 10))
	}
	http.ServeContent(w, r, "", f.ModTime(), f)
}

// DELETE /attachments/{id} (whoever added it, or update on the record's type): remove a file, or cancel an upload
// in flight. The file is removed from storage soon after.
func (h *handlers) deleteAttachment(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		a, err := q.GetAttachment(r.Context(), id)
		if err != nil {
			return err
		}
		t, err := q.GetTask(r.Context(), a.TaskID)
		if err != nil {
			return err
		}
		if !canDeleteAttachment(p, a.CreatedBy, t.Type) {
			return forbidden(fmt.Sprintf("removing a file someone else attached needs %s", authz.Record(authz.Update, t.Type)))
		}
		if n, err := q.DeleteAttachment(r.Context(), id); err != nil {
			return err
		} else if n == 0 {
			return forbidden("you may not remove that file")
		}
		return nil
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
