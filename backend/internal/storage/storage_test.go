package storage_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/storage"
	"github.com/servicelater/servicelater/backend/internal/storage/storagetest"
)

func TestPlanKeepsWithinS3Limits(t *testing.T) {
	for _, size := range []int64{1, storage.MinPartSize, storage.MinPartSize + 1, 10 << 30, 5 << 40} {
		ps, n := storage.Plan(size)
		if ps < storage.MinPartSize || n < 1 || n > storage.MaxParts || int64(n-1)*ps >= size || int64(n)*ps < size || ps > 5<<30 {
			t.Errorf("Plan(%d) = %d x %d", size, ps, n)
		}
		off, l := storage.PartBounds(size, ps, n)
		if off+l != size || l <= 0 {
			t.Errorf("last part of %d: %d+%d", size, off, l)
		}
	}
}

func TestSignedLinksAreBoundToTheirPurposeAndExpire(t *testing.T) {
	s, other := storage.NewSigner("a-secret-a-secret-a-secret-a-secret"), storage.NewSigner("another-secret-another-secret-xx")
	id, now := uuid.New(), time.Now()
	tok := s.Sign(storage.ForPart, id, 3, now.Add(time.Minute))
	if !s.Verify(tok, storage.ForPart, id, 3, now) {
		t.Fatal("a good token is refused")
	}
	for what, ok := range map[string]bool{
		"another part":       s.Verify(tok, storage.ForPart, id, 4, now),
		"another file":       s.Verify(tok, storage.ForPart, uuid.New(), 3, now),
		"another purpose":    s.Verify(tok, storage.ForDownload, id, 3, now),
		"another server key": other.Verify(tok, storage.ForPart, id, 3, now),
		"after it expires":   s.Verify(tok, storage.ForPart, id, 3, now.Add(2*time.Minute)),
		"a later expiry":     s.Verify(strings.Replace(tok, tok[:strings.Index(tok, ".")], "zzzzzzz", 1), storage.ForPart, id, 3, now),
		"garbage":            s.Verify("x.y", storage.ForPart, id, 3, now),
	} {
		if ok {
			t.Errorf("a token is accepted for %s", what)
		}
	}
}

func random(t *testing.T, n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// upload sends data through a store as the browser would through the server: every part, at once, in any order.
func upload(t *testing.T, s storage.Store, key string, data []byte) error {
	t.Helper()
	ctx := context.Background()
	size := int64(len(data))
	ps, n := storage.Plan(size)
	id, err := s.Begin(ctx, key, "application/octet-stream")
	if err != nil {
		return err
	}
	parts := make([]storage.Part, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := n; i >= 1; i-- {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off, l := storage.PartBounds(size, ps, i)
			etag, err := s.PutPart(ctx, key, id, i, off, l, bytes.NewReader(data[off:off+l]))
			parts[i-1], errs[i-1] = storage.Part{Number: i, ETag: etag}, err
		}(i)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return s.Complete(ctx, key, id, parts, size)
}

func readAll(t *testing.T, s storage.Store, key string) []byte {
	t.Helper()
	f, err := s.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	// Seek as http.ServeContent does for a range.
	if _, err := f.Seek(5, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	tail, _ := io.ReadAll(f)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 5)
	_, _ = io.ReadFull(f, head)
	return append(head, tail...)
}

func checkStore(t *testing.T, s storage.Store) {
	ctx := context.Background()
	if err := s.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	// Several parts (the last a short one), and a file of one small part.
	for _, size := range []int{2*storage.MinPartSize + 123, 10} {
		data := random(t, size)
		key := storage.Key("", uuid.New())
		if err := upload(t, s, key, data); err != nil {
			t.Fatalf("%d bytes: %v", size, err)
		}
		if got := readAll(t, s, key); !bytes.Equal(got, data) {
			t.Fatalf("%d bytes came back different (%d bytes)", size, len(got))
		}
		if err := s.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Open(ctx, key); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("a deleted file opens: %v", err)
		}
		if err := s.Delete(ctx, key); err != nil {
			t.Errorf("deleting twice: %v", err)
		}
	}
	// A part longer or shorter than it should be is refused.
	key := storage.Key("", uuid.New())
	id, err := s.Begin(ctx, key, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPart(ctx, key, id, 1, 0, 10, strings.NewReader("too short")); !errors.Is(err, storage.ErrSize) {
		t.Errorf("a short part: %v", err)
	}
	if err := s.Abort(ctx, key, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPart(ctx, key, id, 1, 0, 3, strings.NewReader("abc")); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a part of an aborted upload: %v", err)
	}
}

func TestLocalStore(t *testing.T) {
	dir := t.TempDir()
	checkStore(t, &storage.Local{Root: dir})
	// Completing with the file short of its size is refused, and the upload stays to be aborted.
	s, ctx := &storage.Local{Root: dir}, context.Background()
	key := storage.Key("", uuid.New())
	_, _ = s.Begin(ctx, key, "")
	if _, err := s.PutPart(ctx, key, "", 1, 0, 3, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, key, "", []storage.Part{{Number: 1, ETag: "x"}}, 4); !errors.Is(err, storage.ErrSize) {
		t.Errorf("completing short: %v", err)
	}
	// Keys never leave the folder.
	if _, err := s.Open(ctx, "../etc/passwd"); err == nil || errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a key outside the folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(key)+".part")); err != nil {
		t.Errorf("the unfinished upload is gone: %v", err)
	}
}

func TestS3Store(t *testing.T) {
	cfg := storagetest.StartS3(t)
	s, err := storage.NewS3(cfg, "secret")
	if err != nil {
		t.Fatal(err)
	}
	checkStore(t, s)

	// Directly: the browser PUTs parts to presigned URLs and reports their ETags.
	ctx, data := context.Background(), random(t, storage.MinPartSize+7)
	key := storage.Key("", uuid.New())
	id, err := s.Begin(ctx, key, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	ps, n := storage.Plan(int64(len(data)))
	var parts []storage.Part
	for i := 1; i <= n; i++ {
		off, l := storage.PartBounds(int64(len(data)), ps, i)
		u, err := s.PartURL(ctx, key, id, i, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(data[off:off+l]))
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 200 || res.Header.Get("ETag") == "" {
			t.Fatalf("PUT part %d: %v %v", i, err, res)
		}
		_ = res.Body.Close()
		parts = append(parts, storage.Part{Number: i, ETag: res.Header.Get("ETag")})
	}
	// Claiming another size than what arrived drops the file.
	if err := s.Complete(ctx, key, id, parts, int64(len(data))+1); !errors.Is(err, storage.ErrSize) {
		t.Fatalf("completing with the wrong size: %v", err)
	}
	if _, err := s.Open(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a file of the wrong size is kept: %v", err)
	}

	key = storage.Key("", uuid.New())
	if err := upload(t, s, key, data); err != nil {
		t.Fatal(err)
	}
	u, err := s.DownloadURL(ctx, key, "report 2026.png", "image/png", true, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if !bytes.Equal(got, data) {
		t.Errorf("the download differs (%d bytes, status %d)", len(got), res.StatusCode)
	}
}

func TestOnlyPicturesAreShownInline(t *testing.T) {
	for ct, want := range map[string]string{"image/png": "image/png", "IMAGE/JPEG": "image/jpeg", "text/html": "application/octet-stream",
		"image/svg+xml": "application/octet-stream", "application/pdf": "application/octet-stream"} {
		got, disp := storage.Headers("a.x", ct, true)
		if got != want || strings.HasPrefix(disp, "inline") != (want != "application/octet-stream") {
			t.Errorf("%s: %s, %s", ct, got, disp)
		}
	}
	if _, disp := storage.Headers("résumé \"final\".pdf", "application/pdf", false); !strings.HasPrefix(disp, "attachment;") ||
		!strings.Contains(disp, "filename*=utf-8''r%C3%A9sum%C3%A9") {
		t.Errorf("a name that needs encoding: %s", disp)
	}
	if storage.ContentType("not a type") != "application/octet-stream" || storage.ContentType("text/plain; charset=utf-8") != "text/plain" {
		t.Error("content types are not normalised")
	}
}
