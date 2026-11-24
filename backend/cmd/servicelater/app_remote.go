package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/servicelater/servicelater/backend/internal/apps"
)

// An instance's apps API (Admin → Apps), reached with an API key whose account holds apps:manage: what
// `app install -url` and `app dev` talk to, so neither needs the database.

// remote is one instance's apps API.
type remote struct {
	url, key string
	http     *http.Client
}

func newRemote(url string) (*remote, error) {
	key := os.Getenv("SL_API_KEY")
	if key == "" {
		return nil, errors.New("set SL_API_KEY to an API key whose service account holds apps:manage (Admin → Users → the account → API keys)")
	}
	return &remote{url: strings.TrimRight(url, "/"), key: key, http: &http.Client{Timeout: 2 * time.Minute}}, nil
}

// remotePlan is the instance's answer: what an install does or did.
type remotePlan struct {
	App            string   `json:"app"`
	FromVersion    string   `json:"from_version"`
	ToVersion      string   `json:"to_version"`
	Added          []string `json:"added"`
	Changed        []string `json:"changed"`
	Removed        []string `json:"removed"`
	Renames        []string `json:"renames"`
	Permissions    []string `json:"permissions"`
	NewPermissions []string `json:"new_permissions"`
	RunsAsApp      []string `json:"runs_as_app"`
	Notes          []string `json:"notes"`
	NoChange       bool     `json:"no_change"`
}

func (p remotePlan) String() string {
	var b strings.Builder
	switch {
	case p.FromVersion == "":
		fmt.Fprintf(&b, "%s %s (new)\n", p.App, p.ToVersion)
	case p.NoChange:
		fmt.Fprintf(&b, "%s %s (no changes)\n", p.App, p.ToVersion)
	default:
		fmt.Fprintf(&b, "%s %s -> %s\n", p.App, p.FromVersion, p.ToVersion)
	}
	for _, l := range []struct {
		label string
		items []string
	}{{"add", p.Added}, {"change", p.Changed}, {"remove", p.Removed}, {"rename", p.Renames}} {
		for _, x := range l.items {
			fmt.Fprintf(&b, "  %-10s %s\n", l.label, x)
		}
	}
	if len(p.Permissions) > 0 {
		fmt.Fprintf(&b, "  %-10s %s\n", "permits", strings.Join(p.Permissions, ", "))
	}
	if len(p.NewPermissions) > 0 {
		fmt.Fprintf(&b, "  %-10s %s (needs approval)\n", "new", strings.Join(p.NewPermissions, ", "))
	}
	for _, r := range p.RunsAsApp {
		fmt.Fprintf(&b, "  %-10s rule %s acts as the app\n", "run_as", r)
	}
	for _, n := range p.Notes {
		fmt.Fprintf(&b, "  note: %s\n", n)
	}
	return b.String()
}

// refusal is the instance saying no, with every problem it found.
type refusal struct {
	Status   int
	Message  string   `json:"error"`
	Problems []string `json:"problems"`
}

func (e *refusal) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	for _, p := range e.Problems {
		msg += "\n  " + p
	}
	return msg
}

// send posts an app archive to path (/plan or /install with its query) and decodes the plan.
func (rm *remote) send(ctx context.Context, path string, archive []byte) (remotePlan, error) {
	var out remotePlan
	// The address is the one the person running the command gave (-url): reaching it is the point.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rm.url+"/api/v1/apps"+path, bytes.NewReader(archive)) //nolint:gosec // see above
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+rm.key)
	req.Header.Set("Content-Type", "application/zip")
	res, err := rm.http.Do(req) //nolint:gosec // the operator's own -url
	if err != nil {
		return out, fmt.Errorf("cannot reach %s: %w", rm.url, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return out, err
	}
	if res.StatusCode != http.StatusOK {
		e := &refusal{Status: res.StatusCode}
		if json.Unmarshal(body, e) != nil || e.Message == "" {
			e.Message = fmt.Sprintf("%s answered %s", rm.url, res.Status)
		}
		return out, e
	}
	return out, json.Unmarshal(body, &out)
}

// archiveOf is what to send for a path: a folder is packed (its shipped layer, checked here first), a .swapp sent as
// it is.
func archiveOf(p string) ([]byte, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		_, data, err := apps.Pack(p)
		return data, err
	}
	return os.ReadFile(p) //nolint:gosec // the person running the command names the file to send
}

// appInstallRemote is `app install -url`: plan (dry run) or install each app on an instance, through its API.
func appInstallRemote(ctx context.Context, url string, dry, approve bool, paths []string, out io.Writer) error {
	rm, err := newRemote(url)
	if err != nil {
		return err
	}
	for _, p := range paths {
		archive, err := archiveOf(p)
		if err != nil {
			return fmt.Errorf("%s:\n%w", p, err)
		}
		path := "/install"
		switch {
		case dry:
			path = "/plan"
		case approve:
			path += "?approve_permissions=true"
		}
		plan, err := rm.send(ctx, path, archive)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		_, _ = fmt.Fprint(out, plan)
	}
	if dry {
		_, _ = fmt.Fprintln(out, "dry run: nothing was changed")
	}
	return nil
}

// appDev is `app dev`: install a folder into a development instance, then again every time a file in it changes,
// until interrupted. A change that does not compile is reported and the instance keeps the last good version.
func appDev(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("dev", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	url := fs.String("url", os.Getenv("SL_URL"), "")
	approve := fs.Bool("approve-permissions", false, "")
	interval := fs.Duration("interval", 500*time.Millisecond, "")
	once := fs.Bool("once", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *url == "" {
		return errors.New("usage: servicelater app dev -url <instance> [-approve-permissions] <folder>\n\n" +
			"Installs the folder into a development instance (Admin → Instance: development), then again on every save.\n" +
			"The API key comes from SL_API_KEY: a service account's key that holds apps:manage (and roles:manage to\n" +
			"approve the app's permissions); -url may also come from SL_URL")
	}
	dir := fs.Arg(0)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a folder: app dev watches an app's folder", dir)
	}
	rm, err := newRemote(*url)
	if err != nil {
		return err
	}
	path := "/install?dev=true"
	if *approve {
		path += "&approve_permissions=true"
	}
	push := func() error {
		archive, err := archiveOf(dir)
		if err == nil {
			var plan remotePlan
			if plan, err = rm.send(ctx, path, archive); err == nil {
				_, _ = fmt.Fprintf(out, "%s installed\n%s", time.Now().Format("15:04:05"), plan)
				return nil
			}
		}
		_, _ = fmt.Fprintf(out, "%s not installed: %v\n", time.Now().Format("15:04:05"), err)
		var r *refusal
		if errors.As(err, &r) && (r.Status == http.StatusUnauthorized || r.Status == http.StatusForbidden || r.Status == http.StatusConflict && strings.Contains(r.Message, "development")) {
			return err // nothing a save can fix
		}
		return nil
	}
	last, err := fingerprint(dir)
	if err != nil {
		return err
	}
	if err := push(); err != nil {
		return err
	}
	if *once {
		return nil
	}
	_, _ = fmt.Fprintf(out, "watching %s (Ctrl-C to stop)\n", dir)
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		now, err := fingerprint(dir)
		if err != nil {
			_, _ = fmt.Fprintf(out, "cannot read %s: %v\n", dir, err)
			continue
		}
		if now == last {
			continue
		}
		last = now
		if err := push(); err != nil {
			return err
		}
	}
}

// fingerprint sums the names, sizes and times of the visible files in a folder, so a save, a new file or a deleted
// one changes it. Polling needs nothing from the operating system and works in containers and on network folders.
func fingerprint(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00%d\n", p, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return fmt.Sprintf("%x", h.Sum(nil)), err
}
