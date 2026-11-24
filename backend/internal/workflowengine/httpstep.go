package workflowengine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

const (
	// maxHTTPAnswer is the most of an answer read; maxHTTPKept the most kept in vars.
	maxHTTPAnswer = 1 << 20
	maxHTTPKept   = 64 << 10
	// httpBudget bounds an http.request with its retries, inside the step's own time limit.
	httpBudget = 25 * time.Second
)

// errPrivate is a connection refused because it would reach this machine or a private network.
var errPrivate = errors.New("the address is on a private network, which workflows may not call")

// publicOnly refuses to connect to loopback, private, link-local and other non-public addresses. It runs on the
// address actually dialled, so a name that resolves somewhere else later cannot get round it.
func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() ||
		netip.MustParsePrefix("100.64.0.0/10").Contains(ip) {
		return errPrivate
	}
	return nil
}

// httpClient is the client http.request uses: no redirects followed (the next host might not be allowed), and no
// private addresses unless the instance allows them (SL_HTTP_ALLOW_PRIVATE, for tests and private deployments).
func (e *Engine) httpClient() *http.Client {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client != nil {
		return e.client
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !e.AllowPrivateHTTP {
		dialer.Control = publicOnly
	}
	e.client = &http.Client{
		Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second, MaxIdleConnsPerHost: 4,
			IdleConnTimeout: 90 * time.Second, ForceAttemptHTTP2: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return e.client
}

// doHTTP calls an outside API. The host must be one the app's manifest allows (as installed now, so taking it away
// stops calls at once), and a secret one it declares. A network fault, a 429 or a 5xx is tried again (retries times,
// backing off); the answer's status and body land in <step>_status, <step>_ok and <step>_body (parsed when JSON),
// and a call that does not succeed goes to on_error, or fails the run. Each try carries the same Idempotency-Key,
// so the other side can tell a retry of this step (a job retried after a crash) from a new call.
func (e *Engine) doHTTP(ctx context.Context, ex dbgen.WorkflowExecution, st *apps.Step, in expr.Input, out outcome) (outcome, error) {
	var manifest apps.Manifest
	var hold settings.Clone
	if err := db.WithSystem(ctx, e.Pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		a, err := q.GetApp(ctx, ex.App)
		if err != nil {
			return err
		}
		if hold, err = settings.ReadClone(ctx, q); err != nil {
			return err
		}
		return json.Unmarshal(a.Manifest, &manifest)
	}); err != nil {
		return out, fmt.Errorf("reading the %s app's manifest: %w", ex.App, err)
	}
	if hold.HoldHTTP {
		// The safety switch after a clone: production's data must not reach production's partners from here. It fails
		// like a call that could not be made, so on_error carries the run on.
		reason := "outbound calls are off after a clone: an admin turns them back on under Admin → Clone in"
		out = withVars(out, map[string]any{st.ID + "_status": 0, st.ID + "_error": reason})
		out.kind, out.detail["error"] = "failed", reason
		if st.OnError != "" {
			out.next = st.OnError
			return out, nil
		}
		return out, failf("%s", reason)
	}

	x, err := value(ctx, st.URL, in)
	if err != nil {
		return out, failf("url could not be evaluated: %v", err)
	}
	raw, _ := x.(string)
	u, err := url.Parse(raw)
	switch {
	case err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "":
		return out, failf("url %q is not an http(s) address", raw)
	case !apps.HostAllowed(apps.HTTPHosts(manifest.Permissions), u.Hostname()):
		return out, failf("%s is not a host the %s app may call: add http:%s to its manifest's permissions", u.Hostname(), ex.App, u.Hostname())
	}
	out.detail["method"], out.detail["host"] = st.Method, u.Host

	header := http.Header{}
	for _, nv := range st.Headers {
		v, err := value(ctx, nv.Value, in)
		if err != nil {
			return out, failf("headers.%s could not be evaluated: %v", nv.Name, err)
		}
		header.Set(nv.Name, fmt.Sprint(v))
	}
	var body []byte
	if st.Body != nil {
		v, err := jsonValue(ctx, st.Body, in)
		if err != nil {
			return out, failf("body could not be evaluated: %v", err)
		}
		if s, ok := v.(string); ok {
			body = []byte(s)
			if header.Get("Content-Type") == "" {
				header.Set("Content-Type", "text/plain; charset=utf-8")
			}
		} else {
			if body, err = json.Marshal(v); err != nil {
				return out, failf("body cannot be sent as JSON: %v", err)
			}
			if header.Get("Content-Type") == "" {
				header.Set("Content-Type", "application/json")
			}
		}
	}
	if st.Secret != "" {
		declared := slices.ContainsFunc(manifest.Config, func(c apps.ConfigItem) bool { return c.Key == st.Secret && c.Type == "secret" })
		if !declared {
			return out, failf("the %s app declares no secret %q", ex.App, st.Secret)
		}
		var secret string
		err := db.WithSystem(ctx, e.Pool, func(tx pgx.Tx) error {
			var err error
			secret, err = secrets.Get(ctx, dbgen.New(tx), e.Keys, apps.AppSecretName(ex.App, st.Secret))
			return err
		})
		if errors.Is(err, secrets.ErrNotSet) {
			return out, failf("the %s app's secret %s is not set: an admin sets it under Admin → Connections", ex.App, st.Secret)
		}
		if err != nil {
			return out, failf("the %s app's secret %s cannot be read: %v", ex.App, st.Secret, err)
		}
		switch st.Auth {
		case "bearer":
			header.Set("Authorization", "Bearer "+secret)
		case "basic":
			header.Set("Authorization", "Basic "+basicValue(secret))
		case "header":
			header.Set(st.AuthHeader, secret)
		}
	}
	header.Set("Idempotency-Key", fmt.Sprintf("servicelater-%s-%s-%d", ex.ID, st.ID, ex.StepsRun))
	header.Set("User-Agent", "ServiceLater-workflow")

	ctx, cancel := context.WithTimeout(ctx, httpBudget)
	defer cancel()
	client := e.httpClient()
	var (
		status  int
		answer  []byte
		ctype   string
		lastErr error
	)
	for attempt := 0; attempt <= st.Retries; attempt++ {
		if attempt > 0 {
			wait := time.Duration(500<<(attempt-1)) * time.Millisecond
			select {
			case <-ctx.Done():
				attempt = st.Retries + 1
				continue
			case <-time.After(wait):
			}
		}
		reqCtx, done := context.WithTimeout(ctx, st.HTTPTimeout())
		req, err := http.NewRequestWithContext(reqCtx, st.Method, u.String(), bytes.NewReader(body))
		if err != nil {
			done()
			return out, failf("the request cannot be made: %v", err)
		}
		req.Header = header.Clone()
		resp, err := client.Do(req)
		if err != nil {
			done()
			lastErr, status = err, 0
			if errors.Is(err, errPrivate) {
				break // no retry changes where it points
			}
			continue
		}
		answer, err = io.ReadAll(io.LimitReader(resp.Body, maxHTTPAnswer))
		_ = resp.Body.Close()
		done()
		status, ctype, lastErr = resp.StatusCode, resp.Header.Get("Content-Type"), err
		out.detail["tries"] = attempt + 1
		if err == nil && status != http.StatusTooManyRequests && status < 500 {
			break
		}
	}

	set := map[string]any{st.ID + "_status": int64(status), st.ID + "_ok": status >= 200 && status < 300}
	if len(answer) > 0 {
		var parsed any
		if strings.Contains(ctype, "json") && json.Unmarshal(answer, &parsed) == nil {
			if kept, err := json.Marshal(parsed); err == nil && len(kept) <= maxHTTPKept {
				set[st.ID+"_body"] = wholeJSON(parsed)
			} else {
				set[st.ID+"_body"] = string(answer[:min(len(answer), maxHTTPKept)])
			}
		} else {
			set[st.ID+"_body"] = string(answer[:min(len(answer), maxHTTPKept)])
		}
	}
	out.detail["status"] = status
	if status >= 200 && status < 300 {
		return withVars(out, set), nil
	}
	reason := fmt.Sprintf("%s %s answered %d", st.Method, u.Host+u.Path, status)
	if status == 0 {
		reason = fmt.Sprintf("%s %s could not be reached: %v", st.Method, u.Host+u.Path, lastErr)
	} else if len(answer) > 0 {
		reason += ": " + strings.TrimSpace(string(answer[:min(len(answer), 300)]))
	}
	set[st.ID+"_error"] = reason
	out = withVars(out, set)
	out.kind, out.detail["error"] = "failed", reason // the step failed; on_error, if any, carries the run on
	if st.OnError != "" {
		out.next = st.OnError
		return out, nil
	}
	return out, failf("%s", reason)
}

// basicValue is user:password as Basic authentication sends it. A secret without a colon is the user name.
func basicValue(secret string) string {
	if !strings.Contains(secret, ":") {
		secret += ":"
	}
	return base64.StdEncoding.EncodeToString([]byte(secret))
}

// wholeJSON turns JSON's whole floats into int64, as definitions keep integers.
func wholeJSON(x any) any {
	switch v := x.(type) {
	case float64:
		if v == float64(int64(v)) {
			return int64(v)
		}
	case []any:
		for i := range v {
			v[i] = wholeJSON(v[i])
		}
	case map[string]any:
		for k := range v {
			v[k] = wholeJSON(v[k])
		}
	}
	return x
}
