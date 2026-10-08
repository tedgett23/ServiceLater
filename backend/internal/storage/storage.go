// Package storage keeps attachment files: in a folder (Local) or an S3-compatible bucket (S3). Postgres holds what a
// file is and its key; the bytes are only ever here.
//
// Every upload, to either backend, is in parts (Plan): the browser sends each part with its own request, several at
// once, and then the upload is completed. To S3 with direct uploads on, the parts go straight to the bucket on
// presigned URLs; otherwise to the server, which writes them at their offset in the folder or passes them on to the
// bucket. So a large file never has to fit in one request, and the two backends work the same way for the browser.
package storage

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
)

// Parts: every part but the last is the same size, at least MinPartSize (S3 needs 5 MiB), and there are at most
// MaxParts of them (S3's limit), so the part size grows with the file.
const (
	MinPartSize = 16 << 20
	MaxParts    = 10000
)

var (
	// ErrNotFound means the file (or the upload in flight) is not in storage.
	ErrNotFound = errors.New("the file is not in storage")
	// ErrSize means what arrived is not the size it was said to be.
	ErrSize = errors.New("the file is not the size it was said to be")
)

// Plan is how a file of size bytes is uploaded: the size of each part but the last, and how many parts.
func Plan(size int64) (partSize int64, parts int) {
	partSize = MinPartSize
	if need := (size + MaxParts - 1) / MaxParts; need > partSize {
		partSize = (need + (1<<20 - 1)) &^ (1<<20 - 1) // whole MiB
	}
	return partSize, int((size + partSize - 1) / partSize)
}

// PartBounds is where part n (from 1) starts in the file and how long it is.
func PartBounds(size, partSize int64, n int) (offset, length int64) {
	offset = int64(n-1) * partSize
	return offset, min(partSize, size-offset)
}

// Key is where a new attachment's file goes, under prefix (an S3 bucket's, or empty): spread over 256 folders by
// the first two hex digits of its id, so no folder grows huge.
func Key(prefix string, id uuid.UUID) string {
	s := id.String()
	return prefix + "attachments/" + s[:2] + "/" + s
}

// Part is a part of a finished upload, as the browser reports it: its number and the ETag storage answered.
type Part struct {
	Number int    `json:"number"`
	ETag   string `json:"etag"`
}

// File is a stored file opened for reading, with what http.ServeContent needs.
type File interface {
	io.ReadSeekCloser
	Size() int64
	ModTime() time.Time
}

// Store is a place files are kept. Keys come from Key; uploads are identified by the key and, for S3, the upload id
// Begin returns.
type Store interface {
	// Begin starts an upload and returns its id (empty for a folder).
	Begin(ctx context.Context, key, contentType string) (uploadID string, err error)
	// PutPart stores part n of an upload, which is size bytes from offset, read from r. It returns the part's ETag.
	PutPart(ctx context.Context, key, uploadID string, n int, offset, size int64, r io.Reader) (etag string, err error)
	// Complete finishes an upload from its parts, which must make size bytes.
	Complete(ctx context.Context, key, uploadID string, parts []Part, size int64) error
	// Abort drops an upload in flight and whatever it stored.
	Abort(ctx context.Context, key, uploadID string) error
	Open(ctx context.Context, key string) (File, error)
	// Delete removes a file; one already gone is not an error.
	Delete(ctx context.Context, key string) error
	// Probe writes, reads back and removes a small file, to show the store works.
	Probe(ctx context.Context) error
}

// Direct is a store browsers can reach themselves: S3 with direct uploads on.
type Direct interface {
	// PartURL is a presigned URL to PUT part n of an upload to.
	PartURL(ctx context.Context, key, uploadID string, n int, expiry time.Duration) (string, error)
	// DownloadURL is a presigned URL to GET a file from, saved under filename (or shown, when inline).
	DownloadURL(ctx context.Context, key, filename, contentType string, inline bool, expiry time.Duration) (string, error)
}
