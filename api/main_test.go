package main

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeGitHub struct {
	head    string
	refuse  bool // answers 422 to reviews with inline comments
	reviews []reviewRequest
	revoked bool
	queries []string
	srv     *httptest.Server
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{head: "abc123"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "good" || r.Form.Get("client_secret") != "secret" {
			writeJSON(w, 200, map[string]string{"error": "bad_verification_code"})
			return
		}
		writeJSON(w, 200, map[string]string{"access_token": "tok"})
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"login": "alice"})
	})
	mux.HandleFunc("GET /repos/keycloak/keycloak/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"state": "open", "head": map[string]string{"sha": g.head}})
	})
	mux.HandleFunc("POST /repos/keycloak/keycloak/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		var req reviewRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if g.refuse && len(req.Comments) > 0 {
			writeJSON(w, 422, map[string]string{"message": "Line could not be resolved"})
			return
		}
		g.reviews = append(g.reviews, req)
		writeJSON(w, 200, map[string]string{"html_url": "https://github.com/keycloak/keycloak/pull/7#pullrequestreview-1"})
	})
	mux.HandleFunc("POST /markdown", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		_, _ = w.Write([]byte("<p>" + in["text"] + "</p>"))
	})
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		g.queries = append(g.queries, r.URL.Query().Get("q"))
		items := []map[string]int{{"number": 7}}
		if strings.Contains(r.URL.Query().Get("q"), "involves:") {
			items = append(items, map[string]int{"number": 3})
		}
		writeJSON(w, 200, map[string]any{"items": items})
	})
	mux.HandleFunc("DELETE /applications/id/token", func(w http.ResponseWriter, r *http.Request) {
		g.revoked = true
		w.WriteHeader(204)
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func setup(t *testing.T) (*fakeGitHub, *httptest.Server, *http.Client) {
	g := newFakeGitHub(t)
	dir := t.TempDir()
	_ = os.Mkdir(filepath.Join(dir, "reviews"), 0o755)
	e := export{SHA: "abc123", Intro: "### AI review", Findings: []finding{
		{ID: 0, Severity: "major", Path: "A.java", Line: 10, InDiff: true, Body: "inline"},
		{ID: 1, Severity: "followup", Path: "B.java", Line: 3, Body: "outside"},
		{ID: 2, Severity: "nit", Path: "A.java", Line: 12, InDiff: true, Body: "nit"},
	}}
	b, _ := json.Marshal(e)
	_ = os.WriteFile(filepath.Join(dir, "reviews", "7.json"), b, 0o644)

	s := &server{clientID: "id", clientSecret: "secret", dataDir: dir, repo: "keycloak/keycloak",
		githubWeb: g.srv.URL, githubAPI: g.srv.URL, client: g.srv.Client(), sessions: map[string]session{}}
	app := httptest.NewTLSServer(s.routes())
	t.Cleanup(app.Close)
	s.publicURL = app.URL
	jar, _ := cookiejar.New(nil)
	c := app.Client()
	c.Jar = jar
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return g, app, c
}

func signIn(t *testing.T, app *httptest.Server, c *http.Client) {
	res, err := c.Get(app.URL + "/auth/login?return=/reviews/7.html")
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	if loc.Query().Get("scope") != "public_repo" || loc.Query().Get("redirect_uri") != app.URL+"/auth/callback" {
		t.Fatalf("authorize URL %s", loc)
	}
	res, err = c.Get(app.URL + "/auth/callback?code=good&state=" + loc.Query().Get("state"))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 302 || res.Header.Get("Location") != "/reviews/7.html" {
		t.Fatalf("callback answered %d to %s", res.StatusCode, res.Header.Get("Location"))
	}
}

func post(t *testing.T, app *httptest.Server, c *http.Client, origin, body string) (*http.Response, map[string]string) {
	req, _ := http.NewRequest("POST", app.URL+"/api/reviews/7", strings.NewReader(body))
	req.Header.Set("Origin", origin)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

func TestPostsPickedCommentsInline(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	res, out := post(t, app, c, app.URL, `{"ids":[0,1]}`)
	if res.StatusCode != 200 || out["url"] == "" {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
	r := g.reviews[0]
	if r.CommitID != "abc123" || r.Event != "COMMENT" || len(r.Comments) != 1 || r.Comments[0].Path != "A.java" || r.Comments[0].Line != 10 {
		t.Fatalf("review %+v", r)
	}
	if !strings.Contains(r.Comments[0].Body, "/reviews/7.html#c0") {
		t.Fatalf("inline comment without its source: %q", r.Comments[0].Body)
	}
	if !strings.Contains(r.Body, "outside") || !strings.Contains(r.Body, "/blob/abc123/B.java#L3") || strings.Contains(r.Body, "nit") {
		t.Fatalf("body %q", r.Body)
	}
}

func TestFallsBackToBodyWhenGitHubRefusesALine(t *testing.T) {
	g, app, c := setup(t)
	g.refuse = true
	signIn(t, app, c)
	if res, out := post(t, app, c, app.URL, `{"ids":[0]}`); res.StatusCode != 200 {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
	if r := g.reviews[0]; len(r.Comments) != 0 || !strings.Contains(r.Body, "inline") {
		t.Fatalf("review %+v", r)
	}
}

func TestRefusesAStaleReview(t *testing.T) {
	g, app, c := setup(t)
	g.head = "def456"
	signIn(t, app, c)
	if res, _ := post(t, app, c, app.URL, `{"ids":[0]}`); res.StatusCode != 409 || len(g.reviews) != 0 {
		t.Fatalf("post answered %d", res.StatusCode)
	}
}

func TestRefusesOtherOriginsAndUnknownSessions(t *testing.T) {
	g, app, c := setup(t)
	if res, _ := post(t, app, c, app.URL, `{"ids":[0]}`); res.StatusCode != 401 {
		t.Fatalf("without session %d", res.StatusCode)
	}
	signIn(t, app, c)
	if res, _ := post(t, app, c, "https://evil.example", `{"ids":[0]}`); res.StatusCode != 403 || len(g.reviews) != 0 {
		t.Fatalf("other origin %d", res.StatusCode)
	}
}

func TestAcceptsSameSiteFetchWithNullOrigin(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	req, _ := http.NewRequest("POST", app.URL+"/api/reviews/7", strings.NewReader(`{"ids":[1]}`))
	req.Header.Set("Origin", "null")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if res, err := c.Do(req); err != nil || res.StatusCode != 200 || len(g.reviews) != 1 {
		t.Fatalf("post answered %v %v", res, err)
	}
}

func TestCallbackChecksState(t *testing.T) {
	_, app, c := setup(t)
	if _, err := c.Get(app.URL + "/auth/login?return=https://evil.example/"); err != nil {
		t.Fatal(err)
	}
	res, _ := c.Get(app.URL + "/auth/callback?code=good&state=wrong")
	if res.StatusCode != 400 {
		t.Fatalf("callback answered %d", res.StatusCode)
	}
}

func TestLogoutRevokesTheToken(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	req, _ := http.NewRequest("POST", app.URL+"/auth/logout", nil)
	req.Header.Set("Origin", app.URL)
	if res, _ := c.Do(req); res.StatusCode != 204 || !g.revoked {
		t.Fatalf("logout answered %d, revoked %v", res.StatusCode, g.revoked)
	}
	res, _ := c.Get(app.URL + "/api/me")
	if res.StatusCode != 401 {
		t.Fatalf("me after logout %d", res.StatusCode)
	}
}

func TestPreviewRendersWhatPostSends(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	req, _ := http.NewRequest("POST", app.URL+"/api/reviews/7/preview", strings.NewReader(`{"ids":[0,1]}`))
	req.Header.Set("Origin", app.URL)
	res, err := c.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("preview answered %v %v", res, err)
	}
	var out struct {
		Body     string `json:"body"`
		Comments []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			HTML string `json:"html"`
		} `json:"comments"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	if !strings.Contains(out.Body, "outside") || len(out.Comments) != 1 || out.Comments[0].Path != "A.java" || !strings.Contains(out.Comments[0].HTML, "inline") {
		t.Fatalf("preview %+v", out)
	}
	if len(g.reviews) != 0 {
		t.Fatal("the preview posted a review")
	}
}

func TestMineSearchesInvolvementAndReviewRequests(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	for range 2 {
		res, err := c.Get(app.URL + "/api/mine")
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("mine answered %v %v", res, err)
		}
		var m struct{ Involved, Requested []int }
		_ = json.NewDecoder(res.Body).Decode(&m)
		if len(m.Involved) != 2 || len(m.Requested) != 1 {
			t.Fatalf("mine %+v", m)
		}
	}
	if len(g.queries) != 2 || g.queries[0] != "repo:keycloak/keycloak involves:alice" || g.queries[1] != "repo:keycloak/keycloak is:pr is:open review-requested:alice" {
		t.Fatalf("queries %q, the second call should come from the cache", g.queries)
	}
}
