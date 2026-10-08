package api_test

import "testing"

func TestEveryoneChoosesTheirOwnTheme(t *testing.T) {
	h, pool := setup(t)
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	priya := newActor(t, h, pool, "priya@x.test", "agent")

	type account struct {
		Theme              string `json:"theme"`
		EmailNotifications bool   `json:"email_notifications"`
	}
	if got := decodeBody[account](t, lena.do(h, "GET", "/api/v1/account", "")); got.Theme != "auto" {
		t.Errorf("a new person's theme is %q, want auto", got.Theme)
	}

	// An end user may set their own; it does not touch their other preferences, or anyone else's theme.
	w := lena.do(h, "PATCH", "/api/v1/account", `{"theme":"dark"}`)
	expect(t, w, 200, "choosing dark")
	if got := decodeBody[account](t, w); got.Theme != "dark" || !got.EmailNotifications {
		t.Errorf("after choosing dark: %+v", got)
	}
	if got := decodeBody[account](t, lena.do(h, "GET", "/api/v1/account", "")); got.Theme != "dark" {
		t.Errorf("dark was not kept: %q", got.Theme)
	}
	if got := decodeBody[account](t, priya.do(h, "GET", "/api/v1/account", "")); got.Theme != "auto" {
		t.Errorf("someone else's theme changed to %q", got.Theme)
	}

	expect(t, lena.do(h, "PATCH", "/api/v1/account", `{"theme":"sepia"}`), 422, "a theme that does not exist")
	expect(t, lena.do(h, "PATCH", "/api/v1/account", `{"theme":"auto"}`), 200, "going back to automatic")
}
