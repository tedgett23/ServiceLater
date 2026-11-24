package clone

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/dbtest"
)

func one[T any](t *testing.T, pool *pgxpool.Pool, q string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func exec(t *testing.T, pool *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// Production (the source) has Pat in the Service Desk and an open incident. UAT (the target) made Pat separately,
// with another id, its own password, a group production does not have, a single sign-on link and an API key, and
// has a tester production has never heard of. Loading production's package with addresses masked: UAT's records are
// production's; Pat is production's Pat (production's id, so the incident's assignee is right) with UAT's password,
// address, groups that exist, link and key; the tester is kept as they were; what cannot follow is reported; the
// safety switch is on; nobody stays signed in.
func TestALoadKeepsTheTargetsOwnPeople(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	w := seed(t, pool)
	exec(t, pool, `INSERT INTO groups (name) VALUES ('Service Desk')`)
	exec(t, pool, `INSERT INTO group_members (group_id, user_id) SELECT id, $1 FROM groups WHERE name = 'Service Desk'`, w.pat)

	p := DefaultProfile
	p.Records, p.Mask = RecordsOpen, Mask{Emails: true, Names: true}
	var pkg bytes.Buffer
	if _, err := Export(ctx, pool, &pkg, Options{Profile: p, ProfileName: "uat", Passphrase: "correct horse battery", Instance: "Prod"}); err != nil {
		t.Fatal(err)
	}

	// Make this database the target.
	exec(t, pool, `TRUNCATE tasks CASCADE`)
	exec(t, pool, `DELETE FROM users WHERE id <> $1`, w.mo)
	exec(t, pool, `DELETE FROM groups`)
	uatPat := one[string](t, pool, `INSERT INTO users (email, name, role) VALUES ('Pat@X.test', 'Pat (UAT)', 'admin') RETURNING id`)
	tess := one[string](t, pool, `INSERT INTO users (email, name, role) VALUES ('tess@uat.test', 'Tess', 'agent') RETURNING id`)
	exec(t, pool, `INSERT INTO user_credentials (user_id, password_hash) VALUES ($1, 'uat-hash'), ($2, 'tess-hash')`, uatPat, tess)
	exec(t, pool, `INSERT INTO groups (name) VALUES ('service desk'), ('UAT testers')`)
	exec(t, pool, `INSERT INTO group_members (group_id, user_id) SELECT id, $1 FROM groups`, uatPat)
	exec(t, pool, `INSERT INTO clone_keep_people (user_id) VALUES ($1)`, uatPat)
	exec(t, pool, `INSERT INTO sso_providers (id, label, issuer, client_id) VALUES ('okta', 'Okta', 'https://okta.test', 'c')`)
	exec(t, pool, `INSERT INTO user_identities (provider_id, subject, user_id, email) VALUES ('okta', 'pat-1', $1, 'pat@x.test')`, uatPat)
	exec(t, pool, `INSERT INTO api_keys (user_id, name, prefix, secret_hash) VALUES ($1, 'ci', 'slk_test', '\x00')`, uatPat)
	exec(t, pool, `INSERT INTO tasks (type, title, requester_id) VALUES ('incident', 'Only in UAT', $1)`, tess)
	exec(t, pool, `INSERT INTO settings (name, value) VALUES ('email.enabled', 'true'), ('instance.name', '"UAT"')`)
	exec(t, pool, `INSERT INTO sessions (user_id, family_id, token_hash, expires_at) VALUES ($1, gen_random_uuid(), '\x01', now() + interval '1 day')`, tess)

	o := LoadOptions{Passphrase: "correct horse battery", KeepPeople: true, KeepStudio: true, Applier: uuid.MustParse(tess)}
	pv, err := Inspect(ctx, pool, bytes.NewReader(pkg.Bytes()), o)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]string{}
	for _, k := range pv.People {
		outcomes[k.Email] = k.Outcome
	}
	if outcomes["pat@x.test"] != "matched" || outcomes["tess@uat.test"] != "added" || len(pv.Problems) != 0 {
		t.Fatalf("preview: people %v, problems %v", pv.People, pv.Problems)
	}
	if !slices.ContainsFunc(pv.Dropped, func(s string) bool { return s == "Pat (UAT): group UAT testers (production does not have it)" }) {
		t.Errorf("preview dropped %v", pv.Dropped)
	}
	if one[int](t, pool, `SELECT count(*) FROM tasks`) != 1 {
		t.Fatal("the preview changed something")
	}

	bad := o
	bad.Passphrase = "wrong horse battery"
	if _, err := Load(ctx, pool, bytes.NewReader(pkg.Bytes()), bad); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	rep, err := Load(ctx, pool, bytes.NewReader(pkg.Bytes()), o)
	if err != nil {
		t.Fatal(err)
	}

	if got := one[string](t, pool, `SELECT string_agg(title, ',' ORDER BY title) FROM tasks`); got != "Disk full,Everything is down" {
		t.Errorf("records %q, want production's open ones", got)
	}
	if one[string](t, pool, `SELECT assignee_id::text FROM tasks WHERE title = 'Disk full'`) != w.pat {
		t.Error("the incident's assignee is not production's Pat")
	}
	if one[int](t, pool, `SELECT count(*) FROM users WHERE id = $1`, uatPat) != 0 {
		t.Error("UAT's own id for Pat survived")
	}
	pat := struct{ email, name, role, hash string }{}
	if err := pool.QueryRow(ctx, `SELECT u.email, u.name, u.role, c.password_hash FROM users u JOIN user_credentials c ON c.user_id = u.id
		WHERE u.id = $1`, w.pat).Scan(&pat.email, &pat.name, &pat.role, &pat.hash); err != nil {
		t.Fatal(err)
	}
	if pat != (struct{ email, name, role, hash string }{"Pat@X.test", "Pat (UAT)", "admin", "uat-hash"}) {
		t.Errorf("Pat is %+v, want UAT's address, name, role and password", pat)
	}
	if got := one[string](t, pool, `SELECT string_agg(g.name, ',') FROM group_members m JOIN groups g ON g.id = m.group_id WHERE m.user_id = $1`, w.pat); got != "Service Desk" {
		t.Errorf("Pat's groups %q, want production's Service Desk", got)
	}
	if one[string](t, pool, `SELECT user_id::text FROM user_identities WHERE subject = 'pat-1'`) != w.pat ||
		one[string](t, pool, `SELECT user_id::text FROM api_keys WHERE name = 'ci'`) != w.pat {
		t.Error("Pat's sign-in link or key did not follow them")
	}
	if one[string](t, pool, `SELECT password_hash FROM user_credentials WHERE user_id = $1`, tess) != "tess-hash" {
		t.Error("the tester was not kept")
	}
	if one[int](t, pool, `SELECT count(*) FROM clone_keep_people WHERE user_id = $1`, w.pat) != 1 {
		t.Error("Pat is no longer on the keep list")
	}
	if one[int](t, pool, `SELECT count(*) FROM users WHERE email = 'sam@x.test' OR email LIKE 'user-%@masked.invalid'`) == 0 {
		t.Error("production's other people did not come (masked)")
	}
	if one[int](t, pool, `SELECT count(*) FROM sessions`) != 0 {
		t.Error("someone is still signed in")
	}
	if one[string](t, pool, `SELECT value::text FROM settings WHERE name = 'email.enabled'`) != "false" ||
		one[string](t, pool, `SELECT value::text FROM settings WHERE name = 'clone.hold_http'`) != "true" ||
		one[string](t, pool, `SELECT value::text FROM settings WHERE name = 'instance.name'`) != `"UAT"` {
		t.Error("the safety switch is not on, or the instance's own settings changed")
	}
	if !slices.Contains(rep.SwitchedOff, "Outbound email") || rep.Identities != 1 || rep.APIKeys != 1 || rep.Rows["tasks"] != 2 {
		t.Errorf("report %+v", rep)
	}
	if len(rep.People) != 2 || !slices.ContainsFunc(rep.Dropped, func(s string) bool { return s == "Pat (UAT): group UAT testers (production does not have it)" }) {
		t.Errorf("report people %+v dropped %v", rep.People, rep.Dropped)
	}
	// The instance still works as one: numbering carries on past production's.
	n := one[int64](t, pool, `INSERT INTO tasks (type, title, requester_id) VALUES ('incident', 'After', $1) RETURNING number`, tess)
	if max := one[int64](t, pool, `SELECT max(number) FROM tasks WHERE title <> 'After' AND type = 'incident'`); n <= max {
		t.Errorf("a new incident got number %d, not past production's %d", n, max)
	}
}
