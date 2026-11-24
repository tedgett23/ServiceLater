package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/servicelater/servicelater/backend/internal/apps"
)

// fakeInstance stands in for an instance's apps API: it records each request and answers with a plan, or with the
// refusal it is given.
type fakeInstance struct {
	mu       sync.Mutex
	requests []string // method, path and query, plus the app's version from the archive
	refuse   string   // a JSON body to answer 409 with
}

func (f *fakeInstance) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer slk_test" {
			http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
			return
		}
		data, _ := io.ReadAll(r.Body)
		b, err := apps.LoadArchive(data)
		if err != nil {
			t.Errorf("the archive: %v", err)
		}
		version := ""
		if b != nil {
			for _, line := range strings.Split(string(b.Files["app.yaml"]), "\n") {
				if v, ok := strings.CutPrefix(line, "version: "); ok {
					version = v
				}
			}
		}
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI()+" "+version)
		refuse := f.refuse
		f.mu.Unlock()
		if refuse != "" {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(refuse))
			return
		}
		_, _ = w.Write([]byte(`{"app":"house","from_version":"1.0.0","to_version":"` + version + `","added":["role helper"],"changed":[],"removed":[],"renames":[],"permissions":[],"new_permissions":[],"runs_as_app":[],"notes":[],"no_change":false}`))
	})
}

func (f *fakeInstance) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func houseApp(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("app.yaml", "apiVersion: 1\nname: house\nversion: "+version+"\nauthor: t\ndescription: \"House.\"\n")
	write("default/roles/helper.yaml", "kind: role\nname: helper\nlabel: Helper\ndescription: \"Helps.\"\npermissions: [users:read]\n")
	return dir
}

// app install -url sends the packed app to the instance's API (a plan for -dry-run), with the key from SL_API_KEY,
// and prints what the instance did.
func TestAppInstallThroughAnInstance(t *testing.T) {
	f := &fakeInstance{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	dir := houseApp(t, "1.2.0")

	t.Setenv("SL_API_KEY", "")
	if _, err := cli(t, "install", "-url", srv.URL, dir); err == nil || !strings.Contains(err.Error(), "SL_API_KEY") {
		t.Errorf("without a key: %v", err)
	}
	t.Setenv("SL_API_KEY", "slk_test")
	out, err := cli(t, "install", "-url", srv.URL, "-dry-run", dir)
	if err != nil || !strings.Contains(out, "house 1.0.0 -> 1.2.0") || !strings.Contains(out, "add        role helper") || !strings.Contains(out, "dry run") {
		t.Errorf("dry run: %v\n%s", err, out)
	}
	if _, err := cli(t, "install", "-url", srv.URL, "-approve-permissions", dir); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /api/v1/apps/plan 1.2.0", "POST /api/v1/apps/install?approve_permissions=true 1.2.0"}
	if got := f.seen(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("requests: %q, want %q", got, want)
	}

	f.refuse = `{"error":"the app cannot be installed","problems":["house 1.0.0 is older than the installed 1.2.0"]}`
	if _, err := cli(t, "install", "-url", srv.URL, dir); err == nil || !strings.Contains(err.Error(), "older than the installed") {
		t.Errorf("a refusal: %v", err)
	}
}

// app dev installs the folder as a development reload, then again when a file changes; a change that does not
// compile is reported and the next good one installs. A refusal no save can fix (not a development instance) stops it.
func TestAppDevReloadsOnSave(t *testing.T) {
	f := &fakeInstance{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	t.Setenv("SL_API_KEY", "slk_test")
	dir := houseApp(t, "1.0.0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- runApp(ctx, []string{"dev", "-url", srv.URL, "-interval", "20ms", dir}, &out) }()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("waiting for %s; requests %q, output:\n%s", what, f.seen(), out.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("the first install", func() bool { return len(f.seen()) == 1 })
	waitFor("watching", func() bool { return strings.Contains(out.String(), "watching") })

	// A broken save is reported, not sent; the next good one is.
	bad := filepath.Join(dir, "default/roles/helper.yaml")
	if err := os.WriteFile(bad, []byte("kind: role\nname: Helper Bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor("the problem", func() bool { return strings.Contains(out.String(), "not installed") })
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte("apiVersion: 1\nname: house\nversion: 1.0.1\nauthor: t\ndescription: \"House.\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("kind: role\nname: helper\nlabel: Helper\ndescription: \"Helps more.\"\npermissions: [users:read]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor("the second install", func() bool { return len(f.seen()) == 2 })
	if got := f.seen(); got[0] != "POST /api/v1/apps/install?dev=true 1.0.0" || got[1] != "POST /api/v1/apps/install?dev=true 1.0.1" {
		t.Errorf("requests: %q", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("stopping: %v", err)
	}

	// Not a development instance: it says so and stops.
	f.refuse = `{"error":"development reload only installs into an instance marked development (Admin → Instance)"}`
	if _, err := cli(t, "dev", "-url", srv.URL, dir); err == nil || !strings.Contains(err.Error(), "marked development") {
		t.Errorf("a production instance: %v", err)
	}
}

// syncBuffer is a bytes.Buffer safe to read while the command writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
