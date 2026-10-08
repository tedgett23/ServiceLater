package api_test

import (
	"strings"
	"testing"
)

type articleBody struct {
	ID        string   `json:"id"`
	Reference string   `json:"reference"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Category  string   `json:"category"`
	Tags      []string `json:"tags"`
	Audience  string   `json:"audience"`
	Status    string   `json:"status"`
	Version   int      `json:"version"`
	Moves     []string `json:"moves"`
	Tickets   []struct {
		Reference string `json:"reference"`
	} `json:"tickets"`
}

type hitBody struct {
	Reference string `json:"reference"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Snippet   string `json:"snippet"`
}

func refsOf(hits []hitBody) string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Title
	}
	return strings.Join(out, ",")
}

// Agents draft articles and send them for review; a knowledge manager publishes them; everyone reads what is
// published for everyone; internal articles are for staff; every save keeps a version.
func TestArticlesAreWrittenReviewedAndPublished(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	manager := newActor(t, h, pool, "km@x.test", "end_user")
	expect(t, admin.do(h, "POST", "/api/v1/roles/knowledge-manager/grants", `{"user_id":"`+manager.id+`"}`), 201, "a knowledge manager")

	a := decodeBody[articleBody](t, agent.do(h, "POST", "/api/v1/kb/articles",
		`{"title":"Clearing a printer jam","body":"Open **tray 2** and pull the paper out gently.","category":" Printing ","tags":["Printer","jam","printer"]}`))
	if a.Status != "draft" || a.Version != 1 || a.Category != "Printing" || strings.Join(a.Tags, ",") != "printer,jam" ||
		strings.Join(a.Moves, ",") != "review" || !strings.HasPrefix(a.Reference, "KB") || a.Audience != "everyone" {
		t.Fatalf("drafted: %+v", a)
	}
	expect(t, lena.do(h, "POST", "/api/v1/kb/articles", `{"title":"Mine"}`), 403, "an end user writing an article")
	expect(t, lena.do(h, "GET", "/api/v1/kb/articles/"+a.Reference, ""), 404, "an end user reading a draft")
	refusal(t, agent.do(h, "POST", "/api/v1/kb/articles", `{"title":" "}`), 422, "title must be 1 to 200 characters", "no title")
	refusal(t, agent.do(h, "POST", "/api/v1/kb/articles", `{"title":"x","audience":"friends"}`), 422, "audience must be everyone or internal", "a bad audience")

	expect(t, agent.do(h, "POST", "/api/v1/kb/articles/"+a.ID+"/status", `{"to":"review"}`), 200, "sending it for review")
	refusal(t, agent.do(h, "POST", "/api/v1/kb/articles/"+a.ID+"/status", `{"to":"published"}`), 403, "knowledge:publish", "an agent publishing")
	pub := decodeBody[articleBody](t, manager.do(h, "POST", "/api/v1/kb/articles/"+a.Reference+"/status", `{"to":"published"}`))
	if pub.Status != "published" || strings.Join(pub.Moves, ",") != "retired" {
		t.Errorf("published: %+v", pub)
	}
	if got := decodeBody[articleBody](t, lena.do(h, "GET", "/api/v1/kb/articles/"+a.Reference, "")); got.Title != a.Title || len(got.Moves) != 0 {
		t.Errorf("an end user reads it, with no moves: %+v", got)
	}

	// Search ranks matches and marks them in a plain-text snippet; suggestions take a sentence.
	decodeBody[articleBody](t, agent.do(h, "POST", "/api/v1/kb/articles", `{"title":"Printer driver install","body":"Download the driver."}`))
	hits := decodeBody[struct{ Items []hitBody }](t, lena.do(h, "GET", "/api/v1/kb/articles?q=paper+jam", "")).Items
	if len(hits) != 1 || hits[0].Title != "Clearing a printer jam" || !strings.Contains(hits[0].Snippet, "⸢paper⸣") {
		t.Errorf("search: %+v", hits)
	}
	sug := decodeBody[struct{ Items []hitBody }](t, lena.do(h, "GET", "/api/v1/kb/suggest?q=My+printer+keeps+jamming!", "")).Items
	if len(sug) != 1 || sug[0].Title != "Clearing a printer jam" {
		t.Errorf("suggestions leave out drafts: %+v", sug)
	}
	if all := decodeBody[struct{ Items []hitBody }](t, agent.do(h, "GET", "/api/v1/kb/articles?status=all", "")).Items; len(all) != 2 {
		t.Errorf("a writer lists every status: %v", refsOf(all))
	}
	expect(t, lena.do(h, "GET", "/api/v1/kb/articles?status=draft", ""), 403, "an end user listing drafts")
	cats := decodeBody[struct {
		Items []struct {
			Name     string
			Articles int
		}
	}](t, lena.do(h, "GET", "/api/v1/kb/categories", "")).Items
	if len(cats) != 1 || cats[0].Name != "Printing" || cats[0].Articles != 1 {
		t.Errorf("categories: %+v", cats)
	}

	// Internal articles are for staff.
	internal := decodeBody[articleBody](t, manager.do(h, "POST", "/api/v1/kb/articles", `{"title":"Printer admin password reset","audience":"internal"}`))
	expect(t, manager.do(h, "POST", "/api/v1/kb/articles/"+internal.ID+"/status", `{"to":"published"}`), 200, "publishing it")
	expect(t, lena.do(h, "GET", "/api/v1/kb/articles/"+internal.ID, ""), 404, "an end user reading an internal article")
	expect(t, agent.do(h, "GET", "/api/v1/kb/articles/"+internal.ID, ""), 200, "an agent reading it")
	if hits := decodeBody[struct{ Items []hitBody }](t, lena.do(h, "GET", "/api/v1/kb/articles?q=printer", "")).Items; strings.Contains(refsOf(hits), "admin password") {
		t.Errorf("an end user finds an internal article: %v", refsOf(hits))
	}

	// A published article is changed by those who publish, and keeps its versions.
	refusal(t, agent.do(h, "PATCH", "/api/v1/kb/articles/"+a.ID, `{"body":"Mine now"}`), 403, "knowledge:publish", "an agent changing a published article")
	v2 := decodeBody[articleBody](t, manager.do(h, "PATCH", "/api/v1/kb/articles/"+a.ID, `{"body":"Open tray 3 instead."}`))
	if v2.Version != 2 || v2.Status != "published" {
		t.Errorf("changed: %+v", v2)
	}
	versions := decodeBody[struct {
		Items []struct{ Version int }
	}](t, lena.do(h, "GET", "/api/v1/kb/articles/"+a.ID+"/versions", "")).Items
	if len(versions) != 2 || versions[0].Version != 2 {
		t.Errorf("versions: %+v", versions)
	}
	if v1 := decodeBody[struct{ Body string }](t, lena.do(h, "GET", "/api/v1/kb/articles/"+a.ID+"/versions/1", "")); !strings.Contains(v1.Body, "tray 2") {
		t.Errorf("version 1: %+v", v1)
	}

	refusal(t, manager.do(h, "DELETE", "/api/v1/kb/articles/"+a.ID, ""), 409, "retire it instead", "deleting a published article")
	expect(t, manager.do(h, "POST", "/api/v1/kb/articles/"+a.ID+"/status", `{"to":"retired"}`), 200, "retiring it")
	expect(t, lena.do(h, "GET", "/api/v1/kb/articles/"+a.ID, ""), 404, "an end user reading a retired article")
	draft := decodeBody[articleBody](t, agent.do(h, "POST", "/api/v1/kb/articles", `{"title":"Never mind"}`))
	expect(t, agent.do(h, "DELETE", "/api/v1/kb/articles/"+draft.ID, ""), 403, "an agent deleting")
	expect(t, manager.do(h, "DELETE", "/api/v1/kb/articles/"+draft.ID, ""), 204, "deleting a draft")
}

// Agents link articles to tickets; the requester sees the ones they may read on their own ticket.
func TestArticlesAreLinkedToTickets(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	publish := func(title, audience string) articleBody {
		a := decodeBody[articleBody](t, admin.do(h, "POST", "/api/v1/kb/articles", `{"title":"`+title+`","audience":"`+audience+`"}`))
		expect(t, admin.do(h, "POST", "/api/v1/kb/articles/"+a.ID+"/status", `{"to":"published"}`), 200, "publishing "+title)
		return a
	}
	public, internal := publish("Resetting your password", "everyone"), publish("Unlocking an account in the directory", "internal")
	tk := decodeBody[typedTicket](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"Locked out"}`))
	expect(t, agent.do(h, "PUT", "/api/v1/tickets/"+tk.ID+"/articles/"+public.ID, ""), 200, "linking the public article")
	expect(t, agent.do(h, "PUT", "/api/v1/tickets/"+tk.Reference+"/articles/"+internal.ID, ""), 200, "linking the internal one")
	expect(t, lena.do(h, "PUT", "/api/v1/tickets/"+tk.ID+"/articles/"+internal.ID, ""), 403, "the requester linking one")

	type withArticles struct {
		Articles []struct{ Reference, Title string }
	}
	if mine := decodeBody[withArticles](t, lena.do(h, "GET", "/api/v1/tickets/"+tk.ID, "")); len(mine.Articles) != 1 || mine.Articles[0].Title != public.Title {
		t.Errorf("the requester sees the article she may read: %+v", mine.Articles)
	}
	if theirs := decodeBody[withArticles](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID, "")); len(theirs.Articles) != 2 {
		t.Errorf("the agent sees both: %+v", theirs.Articles)
	}
	if got := decodeBody[articleBody](t, agent.do(h, "GET", "/api/v1/kb/articles/"+public.ID, "")); len(got.Tickets) != 1 || got.Tickets[0].Reference != tk.Reference {
		t.Errorf("the article lists the ticket for staff: %+v", got.Tickets)
	}
	if got := decodeBody[articleBody](t, lena.do(h, "GET", "/api/v1/kb/articles/"+public.ID, "")); len(got.Tickets) != 0 {
		t.Errorf("not for an end user: %+v", got.Tickets)
	}
	expect(t, agent.do(h, "DELETE", "/api/v1/tickets/"+tk.ID+"/articles/"+public.ID, ""), 200, "unlinking it")
	expect(t, agent.do(h, "DELETE", "/api/v1/tickets/"+tk.ID+"/articles/"+public.ID, ""), 404, "unlinking it again")
}
