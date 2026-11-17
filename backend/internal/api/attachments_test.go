package api_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/api"
	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
	"github.com/servicelater/servicelater/backend/internal/storage"
	"github.com/servicelater/servicelater/backend/internal/storage/storagetest"
)

type filesEnv struct {
	h     http.Handler
	pool  *pgxpool.Pool
	files *storage.Manager
	dir   string
	admin actor
}

// setupFiles is setupApps with a secrets key and an attachments folder of the test's own.
func setupFiles(t *testing.T) filesEnv {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.InstallFirstParty(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM settings WHERE name LIKE 'storage.%'; DELETE FROM secrets WHERE name LIKE 'storage.%'"); err != nil {
		t.Fatal(err)
	}
	reg := apps.NewRegistry(pool)
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	keys, err := secrets.NewKeyring(testKey(7))
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := auth.NewTokens("0123456789abcdef0123456789abcdef", time.Minute)
	svc := auth.NewService(pool, tk, time.Hour)
	dir := t.TempDir()
	files := &storage.Manager{Pool: pool, Keys: keys, LocalDir: dir}
	h := api.NewRouter(api.Deps{Pool: pool, Auth: svc, Tokens: tk, Apps: reg, Keys: keys, Files: files,
		Signer: storage.NewSigner("0123456789abcdef0123456789abcdef")})
	return filesEnv{h: h, pool: pool, files: files, dir: dir, admin: newActor(t, h, pool, "boss@x.test", "admin")}
}

type attachmentBody struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Status      string `json:"status"`
	Inline      bool   `json:"inline"`
	CanDelete   bool   `json:"can_delete"`
}

type ticketFiles struct {
	Attachments []attachmentBody `json:"attachments"`
	CanAttach   bool             `json:"can_attach"`
}

type uploadBody struct {
	Attachment attachmentBody `json:"attachment"`
	PartSize   int64          `json:"part_size"`
	Parts      int            `json:"parts"`
}

type partURLs struct {
	Parts []struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		Size   int64  `json:"size"`
	} `json:"parts"`
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// send makes a request to a link the API handed out: through the router when it is the server's own, over HTTP when
// it is a bucket's. It carries no session.
func (e filesEnv) send(t *testing.T, method, link string, body []byte) (int, http.Header, []byte) {
	t.Helper()
	if strings.HasPrefix(link, "/") {
		w := do(e.h, method, link, string(body), nil)
		return w.Code, w.Header(), w.Body.Bytes()
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, link, bytes.NewReader(body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, b
}

// begin starts an upload as a, and answers it.
func (e filesEnv) begin(t *testing.T, a actor, ticketID, name, ctype string, size int) uploadBody {
	t.Helper()
	w := a.do(e.h, "POST", "/api/v1/tickets/"+ticketID+"/attachments", fmt.Sprintf(`{"filename":%q,"size":%d,"content_type":%q}`, name, size, ctype))
	expect(t, w, 201, "starting an upload of "+name)
	return decodeBody[uploadBody](t, w)
}

// sendParts PUTs every part of an upload and answers the parts to complete it with.
func (e filesEnv) sendParts(t *testing.T, a actor, up uploadBody, data []byte) string {
	t.Helper()
	nums := make([]int, up.Parts)
	for i := range nums {
		nums[i] = i + 1
	}
	raw, _ := json.Marshal(map[string]any{"numbers": nums})
	urls := decodeBody[partURLs](t, a.do(e.h, "POST", "/api/v1/attachments/"+up.Attachment.ID+"/parts", string(raw)))
	var parts []string
	for _, p := range urls.Parts {
		off := int64(p.Number-1) * up.PartSize
		code, hdr, body := e.send(t, "PUT", p.URL, data[off:off+p.Size])
		if code != 200 || hdr.Get("ETag") == "" {
			t.Fatalf("PUT part %d: %d %s", p.Number, code, body)
		}
		parts = append(parts, fmt.Sprintf(`{"number":%d,"etag":%q}`, p.Number, hdr.Get("ETag")))
	}
	return `{"parts":[` + strings.Join(parts, ",") + `]}`
}

// attach uploads a whole file as a, the way the web UI does.
func (e filesEnv) attach(t *testing.T, a actor, ticketID, name, ctype string, data []byte) attachmentBody {
	t.Helper()
	up := e.begin(t, a, ticketID, name, ctype, len(data))
	parts := e.sendParts(t, a, up, data)
	w := a.do(e.h, "POST", "/api/v1/attachments/"+up.Attachment.ID+"/complete", parts)
	expect(t, w, 200, "completing "+name)
	return decodeBody[attachmentBody](t, w)
}

// fetch downloads an attachment as a through the link the API hands out.
func (e filesEnv) fetch(t *testing.T, a actor, id string, inline bool) (http.Header, []byte) {
	t.Helper()
	w := a.do(e.h, "GET", fmt.Sprintf("/api/v1/attachments/%s/download?inline=%t", id, inline), "")
	expect(t, w, 200, "asking to download")
	link := decodeBody[struct {
		URL string `json:"url"`
	}](t, w).URL
	code, hdr, body := e.send(t, "GET", link, nil)
	if code != 200 {
		t.Fatalf("download: %d %s", code, body)
	}
	return hdr, body
}

// runJobs runs the queued jobs of a kind with a handler, as the worker would.
func runJobs(t *testing.T, pool *pgxpool.Pool, kind string, h jobs.Handler) int {
	t.Helper()
	return runJobsWith(t, pool, kind, h, false)
}

// runJobsWith runs them too, and with failuresOK lets a job fail (as a permanent failure would be dead-lettered).
func runJobsWith(t *testing.T, pool *pgxpool.Pool, kind string, h jobs.Handler, failuresOK bool) int {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, "DELETE FROM jobs WHERE kind = $1 RETURNING id, payload", kind)
	if err != nil {
		t.Fatal(err)
	}
	var todo []jobs.Job
	for rows.Next() {
		var j jobs.Job
		if err := rows.Scan(&j.ID, &j.Payload); err != nil {
			t.Fatal(err)
		}
		j.Kind = kind
		todo = append(todo, j)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, j := range todo {
		if err := h(ctx, j); err != nil && !failuresOK {
			t.Fatalf("%s job: %v", kind, err)
		}
	}
	return len(todo)
}

func (e filesEnv) stored(t *testing.T) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(e.dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(p) != storage.MarkerFile {
			out = append(out, filepath.Base(p))
		}
		return nil
	})
	return out
}

// A file attached to a record is seen by whoever sees the record and nobody else, while it uploads only by its
// uploader; the requester and staff attach; removing one is for whoever added it or may update the record.
func TestAttachmentsAreSeenByWhoeverSeesTheRecord(t *testing.T) {
	e := setupFiles(t)
	h := e.h
	alice := newActor(t, h, e.pool, "alice@x.test", "end_user")
	bob := newActor(t, h, e.pool, "bob@x.test", "end_user")
	agent := newActor(t, h, e.pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, alice.do(h, "POST", "/api/v1/tickets", `{"title":"Printer jams"}`))

	if !decodeBody[ticketFiles](t, alice.do(h, "GET", "/api/v1/tickets/"+tk.ID, "")).CanAttach {
		t.Error("the requester is not offered attaching")
	}
	if decodeBody[ticketFiles](t, bob.do(h, "GET", "/api/v1/tickets/"+tk.ID, "")).CanAttach {
		t.Error("someone who cannot see the record is offered attaching")
	}

	// While it uploads, only alice sees it.
	photo := randomBytes(2*storage.MinPartSize + 1000) // three parts
	up := e.begin(t, alice, tk.ID, `C:\Users\alice\jam photo.png`, "image/png", len(photo))
	if up.Attachment.Filename != "jam photo.png" || up.Attachment.Status != "pending" || up.Parts != 3 {
		t.Fatalf("upload: %+v", up)
	}
	if got := decodeBody[ticketFiles](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID, "")).Attachments; len(got) != 0 {
		t.Errorf("the agent sees an upload in flight: %+v", got)
	}
	expect(t, agent.do(h, "POST", "/api/v1/attachments/"+up.Attachment.ID+"/parts", `{"numbers":[1]}`), 404, "the agent asking for its parts")
	expect(t, agent.do(h, "GET", "/api/v1/attachments/"+up.Attachment.ID+"/download", ""), 404, "the agent downloading it unfinished")
	parts := e.sendParts(t, alice, up, photo)
	expect(t, agent.do(h, "POST", "/api/v1/attachments/"+up.Attachment.ID+"/complete", parts), 404, "the agent completing it")
	w := alice.do(h, "POST", "/api/v1/attachments/"+up.Attachment.ID+"/complete", parts)
	expect(t, w, 200, "alice completing it")
	done := decodeBody[attachmentBody](t, w)
	if done.Status != "ready" || !done.Inline || !done.CanDelete {
		t.Errorf("finished: %+v", done)
	}
	expect(t, alice.do(h, "POST", "/api/v1/attachments/"+up.Attachment.ID+"/complete", parts), 409, "completing twice")

	seen := decodeBody[ticketFiles](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID, "")).Attachments
	if len(seen) != 1 || seen[0].Filename != "jam photo.png" || seen[0].Size != int64(len(photo)) || !seen[0].CanDelete {
		t.Fatalf("the agent sees: %+v", seen)
	}
	hdr, body := e.fetch(t, agent, done.ID, true)
	if !bytes.Equal(body, photo) || hdr.Get("Content-Type") != "image/png" || !strings.HasPrefix(hdr.Get("Content-Disposition"), "inline") ||
		hdr.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(hdr.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("download: %d bytes, %v", len(body), hdr)
	}

	// Bob cannot tell it exists, nor attach to the record.
	expect(t, bob.do(h, "GET", "/api/v1/attachments/"+done.ID+"/download", ""), 404, "bob downloading")
	expect(t, bob.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/attachments", `{"filename":"x.txt","size":3}`), 404, "bob attaching")
	expect(t, bob.do(h, "DELETE", "/api/v1/attachments/"+done.ID, ""), 404, "bob removing it")

	// A page that is not a picture is never shown: it is saved, as bytes.
	page := e.attach(t, agent, tk.ID, "logs.html", "text/html", []byte("<script>alert(1)</script>"))
	hdr, _ = e.fetch(t, alice, page.ID, true)
	if hdr.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(hdr.Get("Content-Disposition"), "attachment") {
		t.Errorf("an HTML file is served as %v", hdr)
	}

	// The download link: ranges work, and it cannot be bent to another file or used without its token.
	link := decodeBody[struct {
		URL string `json:"url"`
	}](t, alice.do(h, "GET", "/api/v1/attachments/"+done.ID+"/download", "")).URL
	if w := do(h, "GET", link, "", nil, "Range", "bytes=10-19"); w.Code != 206 || !bytes.Equal(w.Body.Bytes(), photo[10:20]) {
		t.Errorf("a range: %d, %d bytes", w.Code, w.Body.Len())
	}
	expect(t, do(h, "GET", strings.Replace(link, done.ID, page.ID, 1), "", nil), 403, "another file's id on the link")
	expect(t, do(h, "GET", "/api/v1/attachments/"+done.ID+"/file", "", nil), 403, "no token")

	// Removing: alice may not remove the agent's file, but may remove her own.
	if a := decodeBody[ticketFiles](t, alice.do(h, "GET", "/api/v1/tickets/"+tk.ID, "")).Attachments; len(a) != 2 || a[0].CanDelete != true || a[1].CanDelete {
		t.Errorf("what alice may remove: %+v", a)
	}
	refusal(t, alice.do(h, "DELETE", "/api/v1/attachments/"+page.ID, ""), 403, "records:update:incident", "alice removing the agent's file")
	expect(t, alice.do(h, "DELETE", "/api/v1/attachments/"+done.ID, ""), 204, "alice removing her own")
	expect(t, agent.do(h, "GET", "/api/v1/attachments/"+done.ID+"/download", ""), 404, "downloading a removed file")

	// Both are in the record's history, and the removed file leaves storage.
	lines := strings.Join(decodeBody[historyBody](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID+"/history", "")).lines(), "\n")
	for _, want := range []string{"alice@x.test linked Attachment:  -> jam photo.png", "agent@x.test linked Attachment:  -> logs.html",
		"alice@x.test unlinked Attachment: jam photo.png -> "} {
		if !strings.Contains(lines, want) {
			t.Errorf("history lacks %q:\n%s", want, lines)
		}
	}
	if n := len(e.stored(t)); n != 2 {
		t.Errorf("before the purge: %d files", n)
	}
	if n := runJobs(t, e.pool, storage.PurgeKind, e.files.PurgeHandler()); n != 1 {
		t.Errorf("%d purge jobs", n)
	}
	if got := e.stored(t); len(got) != 1 || got[0] != page.ID {
		t.Errorf("after the purge: %v", got)
	}
}

// Each part is checked as it arrives, an upload completes only with every part, and one never finished goes.
func TestUploadsAreCheckedPartByPart(t *testing.T) {
	e := setupFiles(t)
	h := e.h
	agent := newActor(t, h, e.pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Uploads"}`))
	path := "/api/v1/tickets/" + tk.ID + "/attachments"

	refusal(t, agent.do(h, "POST", path, `{"filename":"empty.txt","size":0}`), 422, "the file is empty", "an empty file")
	refusal(t, agent.do(h, "POST", path, `{"filename":"../","size":3}`), 422, "filename is required", "no name")
	refusal(t, agent.do(h, "POST", path, `{"filename":"huge.iso","size":11811160064}`), 422, "files can be at most 10 GiB", "past the default limit")

	up := e.begin(t, agent, tk.ID, "notes.txt", "text/plain; charset=utf-8", 10)
	if up.Attachment.ContentType != "text/plain" {
		t.Errorf("content type: %q", up.Attachment.ContentType)
	}
	refusal(t, agent.do(h, "POST", "/api/v1/attachments/"+up.Attachment.ID+"/parts", `{"numbers":[2]}`), 422, "parts 1 to 1", "a part past the end")
	link := decodeBody[partURLs](t, agent.do(h, "POST", "/api/v1/attachments/"+up.Attachment.ID+"/parts", `{"numbers":[1]}`)).Parts[0].URL
	if code, _, body := e.send(t, "PUT", link, []byte("too long, much too long")); code != 422 || !strings.Contains(string(body), "part 1 is 10 bytes") {
		t.Errorf("a part too long: %d %s", code, body)
	}
	if code, _, _ := e.send(t, "PUT", strings.Replace(link, "/parts/1", "/parts/2", 1), []byte("0123456789")); code != 403 {
		t.Errorf("a part's link used for another part: %d", code)
	}
	// Completing with nothing sent: the file did not arrive whole, so the upload goes.
	refusal(t, agent.do(h, "POST", "/api/v1/attachments/"+up.Attachment.ID+"/complete", `{"parts":[]}`), 422, "this upload has 1 parts", "no parts listed")
	refusal(t, agent.do(h, "POST", "/api/v1/attachments/"+up.Attachment.ID+"/complete", `{"parts":[{"number":1,"etag":"x"}]}`), 422,
		"did not arrive whole", "completing what never arrived")
	expect(t, agent.do(h, "POST", "/api/v1/attachments/"+up.Attachment.ID+"/parts", `{"numbers":[1]}`), 404, "the dropped upload")

	// An upload never finished is removed after a day, file and all.
	stale := e.begin(t, agent, tk.ID, "stale.bin", "", 3)
	if code, _, _ := e.send(t, "PUT", decodeBody[partURLs](t, agent.do(h, "POST", "/api/v1/attachments/"+stale.Attachment.ID+"/parts",
		`{"numbers":[1]}`)).Parts[0].URL, []byte("abc")); code != 200 {
		t.Fatalf("PUT: %d", code)
	}
	if n := countRows(t, e.pool, "SELECT count(*) FROM jobs WHERE kind = $1 AND run_at > now() + interval '23 hours'", storage.ExpireKind); n != 2 {
		t.Errorf("%d expiries queued a day ahead", n)
	}
	runJobs(t, e.pool, storage.ExpireKind, e.files.ExpireHandler())
	runJobs(t, e.pool, storage.PurgeKind, e.files.PurgeHandler())
	if n := countRows(t, e.pool, "SELECT count(*) FROM attachments"); n != 0 {
		t.Errorf("%d attachments left", n)
	}
	if got := e.stored(t); len(got) != 0 {
		t.Errorf("files left: %v", got)
	}

	// Someone may have only so many uploads in flight.
	for i := range 20 {
		e.begin(t, agent, tk.ID, fmt.Sprintf("f%d", i), "", 1)
	}
	refusal(t, agent.do(h, "POST", path, `{"filename":"one more","size":1}`), 429, "20 uploads in flight", "the 21st upload")
	// And cancelling one frees a place.
	var first string
	_ = e.pool.QueryRow(context.Background(), "SELECT id FROM attachments ORDER BY created_at LIMIT 1").Scan(&first)
	expect(t, agent.do(h, "DELETE", "/api/v1/attachments/"+first, ""), 204, "cancelling an upload")
	expect(t, agent.do(h, "POST", path, `{"filename":"one more","size":1}`), 201, "an upload after cancelling one")
}

// The S3 backend, with browsers sending parts straight to the bucket and with the server carrying them; files stay
// readable from the backend they were stored in after the setting changes.
func TestAttachmentsInS3(t *testing.T) {
	e := setupFiles(t)
	h := e.h
	agent := newActor(t, h, e.pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"In a bucket"}`))
	local := e.attach(t, agent, tk.ID, "before.txt", "text/plain", []byte("kept in the folder"))

	cfg := storagetest.StartS3(t)
	put := func(direct bool) {
		cfg.S3Direct = direct
		raw, _ := json.Marshal(cfg)
		body := strings.TrimSuffix(string(raw), "}") + `,"s3_secret_key":"secret"}`
		expect(t, e.admin.do(h, "PUT", "/api/v1/settings/storage", body), 200, "choosing S3")
	}
	for _, direct := range []bool{true, false} {
		put(direct)
		data := randomBytes(storage.MinPartSize + 5)
		up := e.begin(t, agent, tk.ID, fmt.Sprintf("direct-%t.bin", direct), "application/zip", len(data))
		urls := decodeBody[partURLs](t, agent.do(h, "POST", "/api/v1/attachments/"+up.Attachment.ID+"/parts", `{"numbers":[1,2]}`))
		if goesToBucket := strings.HasPrefix(urls.Parts[0].URL, cfg.S3Endpoint); goesToBucket != direct {
			t.Errorf("direct %t: part URL %s", direct, urls.Parts[0].URL)
		}
		w := agent.do(h, "POST", "/api/v1/attachments/"+up.Attachment.ID+"/complete", e.sendParts(t, agent, up, data))
		expect(t, w, 200, "completing in S3")
		if _, body := e.fetch(t, agent, up.Attachment.ID, false); !bytes.Equal(body, data) {
			t.Errorf("direct %t: the download differs", direct)
		}
	}
	if _, body := e.fetch(t, agent, local.ID, false); string(body) != "kept in the folder" {
		t.Errorf("a file stored before the change: %q", body)
	}
	if got := e.stored(t); len(got) != 1 {
		t.Errorf("S3 uploads landed in the folder: %v", got)
	}
	s := decodeBody[struct {
		Usage []struct {
			Backend string `json:"backend"`
			Files   int64  `json:"files"`
		} `json:"usage"`
	}](t, e.admin.do(h, "GET", "/api/v1/settings/storage", ""))
	if fmt.Sprint(s.Usage) != "[{local 1} {s3 2}]" {
		t.Errorf("usage: %+v", s.Usage)
	}
}

// Storage is chosen by those who manage settings; the S3 secret access key is stored encrypted and never read back.
func TestStorageSettings(t *testing.T) {
	e := setupFiles(t)
	h := e.h
	agent := newActor(t, h, e.pool, "agent@x.test", "agent")
	expect(t, agent.do(h, "GET", "/api/v1/settings/storage", ""), 403, "an agent reading storage settings")
	expect(t, agent.do(h, "POST", "/api/v1/settings/storage/test", ""), 403, "an agent testing storage")

	type body struct {
		settings.Storage
		S3SecretKeySet bool   `json:"s3_secret_key_set"`
		LocalDir       string `json:"local_dir"`
	}
	def := decodeBody[body](t, e.admin.do(h, "GET", "/api/v1/settings/storage", ""))
	if def.Backend != "local" || def.MaxSize != 10<<30 || !def.S3Direct || def.LocalDir != e.dir || def.S3SecretKeySet {
		t.Errorf("defaults: %+v", def)
	}
	for b, want := range map[string]string{
		`{"backend":"ftp","max_size":1048576}`:                                                     "backend must be local or s3",
		`{"backend":"local","max_size":10}`:                                                        "max_size must be from 1 MiB",
		`{"backend":"s3","max_size":1048576,"s3_endpoint":"https://s3.amazonaws.com"}`:             "needs an endpoint and a bucket",
		`{"backend":"s3","max_size":1048576,"s3_endpoint":"s3.amazonaws.com","s3_bucket":"files"}`: "s3_endpoint must be",
		`{"backend":"local","max_size":1048576,"s3_bucket":"Bad_Bucket"}`:                          "s3_bucket must be a bucket name",
	} {
		refusal(t, e.admin.do(h, "PUT", "/api/v1/settings/storage", b), 422, want, b)
	}

	res := decodeBody[struct {
		OK       bool     `json:"ok"`
		Detail   string   `json:"detail"`
		Warnings []string `json:"warnings"`
	}](t, e.admin.do(h, "POST", "/api/v1/settings/storage/test", ""))
	if !res.OK {
		t.Errorf("testing the folder: %+v", res)
	}

	cfg := storagetest.StartS3(t)
	raw, _ := json.Marshal(cfg)
	w := e.admin.do(h, "PUT", "/api/v1/settings/storage", strings.TrimSuffix(string(raw), "}")+`,"s3_secret_key":"wrong secret"}`)
	if !decodeBody[body](t, w).S3SecretKeySet || strings.Contains(w.Body.String(), "wrong secret") {
		t.Errorf("saved: %s", w.Body)
	}
	if strings.Contains(e.admin.do(h, "GET", "/api/v1/settings/storage", "").Body.String(), "wrong secret") {
		t.Error("the secret access key is read back")
	}
	// The fake bucket has no CORS rules to read: direct uploads are warned about.
	res = decodeBody[struct {
		OK       bool     `json:"ok"`
		Detail   string   `json:"detail"`
		Warnings []string `json:"warnings"`
	}](t, do(h, "POST", "/api/v1/settings/storage/test", "", e.admin.cookies, "Origin", "https://sl.test"))
	if !res.OK || len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "CORS") {
		t.Errorf("testing the bucket: %+v", res)
	}

	// Someone else is told only that files cannot be stored; the admin is told why.
	expect(t, e.admin.do(h, "PUT", "/api/v1/settings/storage", `{"backend":"local","max_size":1048576}`), 200, "back to the folder")
	if err := os.WriteFile(filepath.Join(e.dir, storage.MarkerFile), []byte("someone else's\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Nowhere to put it"}`))
	w = agent.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/attachments", `{"filename":"a.txt","size":1}`)
	if w.Code != 503 || strings.Contains(w.Body.String(), e.dir) || !strings.Contains(w.Body.String(), "Tell an administrator") {
		t.Errorf("an agent told: %d %s", w.Code, w.Body)
	}
	if w := e.admin.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/attachments", `{"filename":"a.txt","size":1}`); w.Code != 503 ||
		!strings.Contains(w.Body.String(), "not the one the instance keeps its files in") {
		t.Errorf("the admin told: %d %s", w.Code, w.Body)
	}
}

// Files added and removed are always in a record's history, like its status: whatever an admin turns off, which file
// came when, and who removed one, has an answer. Each file says when it joined the record.
func TestFilesAreAlwaysInTheHistory(t *testing.T) {
	e := setupFiles(t)
	h := e.h
	expect(t, e.admin.do(h, "PUT", "/api/v1/settings/history", `{"record_types":[],"cmdb":true}`), 200, "recording only the basics")
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), "DELETE FROM settings WHERE name LIKE 'history.%'")
	})
	agent := newActor(t, h, e.pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Quiet history"}`))
	before := time.Now()
	a := e.attach(t, agent, tk.ID, "first.png", "image/png", []byte("one"))
	b := e.attach(t, agent, tk.ID, "second.txt", "text/plain", []byte("two"))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"title":"Quiet history, renamed"}`), 200, "a change not recorded")
	expect(t, agent.do(h, "DELETE", "/api/v1/attachments/"+a.ID, ""), 204, "removing the first")

	lines := strings.Join(decodeBody[historyBody](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID+"/history", "")).lines(), "\n")
	for _, want := range []string{"linked Attachment:  -> first.png", "linked Attachment:  -> second.txt", "unlinked Attachment: first.png -> "} {
		if !strings.Contains(lines, want) {
			t.Errorf("history lacks %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "renamed") {
		t.Errorf("a title change was recorded though history is off:\n%s", lines)
	}
	got := decodeBody[struct {
		Attachments []struct {
			ID         string     `json:"id"`
			AttachedAt *time.Time `json:"attached_at"`
		} `json:"attachments"`
	}](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID, "")).Attachments
	if len(got) != 1 || got[0].ID != b.ID || got[0].AttachedAt == nil || got[0].AttachedAt.Before(before.Add(-time.Second)) {
		t.Errorf("attachments: %+v", got)
	}
}
