package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeGitHub struct {
	head        string
	closed      bool   // the pull request was closed or merged
	user        string // the login of the person who signs in
	refuse      bool   // answers 422 to reviews with inline comments
	pending     bool   // answers 422 to every review, as when the person has a pending review
	pendingBy   string // lists a pending review by this login, to them only, as GitHub does
	reviewTries int
	reviews     []postedReview
	replies     []postedReply
	failReplies int          // the status of every answer to replies, when set
	replyTries  atomic.Int32 // how many replies the service sent, also refused ones
	replyStatus []int        // the status of the nth reply, when set and not 0; -1 drops the connection
	failReviews int          // the status of every answer to reviews, when set
	failMessage string       // and the message of both, by default the status text
	botRevoked  bool         // answers 401 to the bot token, as after it expired
	// threadComments and issueComments are the comments GitHub lists. Replies that the service posts go there too.
	threadComments []ghComment
	issueComments  []ghComment
	revoked        bool
	queries        []string
	lock           *sync.Mutex // set by tests that call the fake from many goroutines
	srv            *httptest.Server
}

// page returns the page of items that the request asks for with page and per_page, as GitHub does.
func page[T any](r *http.Request, items []T) []T {
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	n, size = max(n, 1), cmp.Or(size, 30)
	from, to := min((n-1)*size, len(items)), min(n*size, len(items))
	return append([]T{}, items[from:to]...)
}

// postedReply is a reply the fake GitHub got: in a thread when thread is set, else as a comment on the pull request.
type postedReply struct {
	thread int
	body   string
	by     string
}

// postedReview is a review the fake GitHub got, with the login of the token that posted it.
type postedReview struct {
	reviewRequest
	by string
}

func (g *fakeGitHub) guard(f func()) {
	if g.lock != nil {
		g.lock.Lock()
		defer g.lock.Unlock()
	}
	f()
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{head: "abc123", user: "alice", threadComments: []ghComment{{ID: 55, Body: "Why?", HTMLURL: "https://github.com/c55"}}}
	logins := func(r *http.Request) string {
		switch r.Header.Get("Authorization") {
		case "Bearer tok":
			return g.user
		case "Bearer bottok":
			if g.botRevoked {
				return ""
			}
			return "tracker-bot"
		}
		return ""
	}
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
		if logins(r) == "" {
			writeJSON(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		writeJSON(w, 200, map[string]string{"login": logins(r)})
	})
	mux.HandleFunc("GET /repos/keycloak/keycloak/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		if logins(r) == "" {
			writeJSON(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		writeJSON(w, 200, map[string]any{"state": map[bool]string{false: "open", true: "closed"}[g.closed], "head": map[string]string{"sha": g.head}})
	})
	mux.HandleFunc("POST /repos/keycloak/keycloak/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		by := logins(r)
		if by == "" {
			w.WriteHeader(401)
			return
		}
		var req reviewRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		g.guard(func() { g.reviewTries++ })
		if g.pending {
			writeJSON(w, 422, map[string]any{"message": "Unprocessable Entity", "errors": []string{"User can only have one pending review per pull request"}})
			return
		}
		if g.failReviews != 0 {
			writeJSON(w, g.failReviews, map[string]string{"message": cmp.Or(g.failMessage, http.StatusText(g.failReviews))})
			return
		}
		if g.refuse && len(req.Comments) > 0 {
			writeJSON(w, 422, map[string]any{"message": "Unprocessable Entity", "errors": []string{"Line could not be resolved"}})
			return
		}
		g.guard(func() { g.reviews = append(g.reviews, postedReview{req, by}) })
		writeJSON(w, 200, map[string]string{"html_url": "https://github.com/keycloak/keycloak/pull/7#pullrequestreview-1"})
	})
	reply := func(thread int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			by := logins(r)
			if by == "" {
				writeJSON(w, 500, map[string]string{"message": "Server Error"})
				return
			}
			if n := int(g.replyTries.Add(1)); n <= len(g.replyStatus) && g.replyStatus[n-1] != 0 {
				switch code := g.replyStatus[n-1]; code {
				case -1:
					conn, _, _ := w.(http.Hijacker).Hijack()
					_ = conn.Close()
				case http.StatusTooManyRequests:
					writeJSON(w, code, map[string]string{"message": "You have exceeded a secondary rate limit."})
				default:
					writeJSON(w, code, map[string]string{"message": http.StatusText(code)})
				}
				return
			}
			if g.failReplies != 0 {
				m := g.failMessage
				if m == "" {
					m = http.StatusText(g.failReplies)
				}
				writeJSON(w, g.failReplies, map[string]string{"message": m})
				return
			}
			var in struct{ Body string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			g.guard(func() {
				g.replies = append(g.replies, postedReply{thread, in.Body, by})
				c := ghComment{ID: int64(1000 + len(g.replies)), Body: in.Body, HTMLURL: "https://github.com/reply" + strconv.Itoa(len(g.replies))}
				if thread != 0 {
					c.InReplyTo = int64(thread)
					g.threadComments = append(g.threadComments, c)
				} else {
					g.issueComments = append(g.issueComments, c)
				}
			})
			writeJSON(w, 201, map[string]string{"html_url": "https://github.com/keycloak/keycloak/pull/7#reply"})
		}
	}
	mux.HandleFunc("POST /repos/keycloak/keycloak/pulls/7/comments/55/replies", reply(55))
	mux.HandleFunc("POST /repos/keycloak/keycloak/issues/7/comments", reply(0))
	list := func(cs *[]ghComment) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if logins(r) == "" {
				writeJSON(w, 401, map[string]string{"message": "Bad credentials"})
				return
			}
			var out []ghComment
			g.guard(func() { out = append(out, *cs...) })
			writeJSON(w, 200, page(r, out))
		}
	}
	mux.HandleFunc("GET /repos/keycloak/keycloak/pulls/7/comments", list(&g.threadComments))
	mux.HandleFunc("GET /repos/keycloak/keycloak/issues/7/comments", list(&g.issueComments))
	mux.HandleFunc("GET /repos/keycloak/keycloak/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		if logins(r) == "" {
			writeJSON(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		out := []map[string]any{}
		g.guard(func() {
			for i, rv := range g.reviews {
				out = append(out, map[string]any{"commit_id": rv.CommitID, "body": rv.Body, "user": map[string]string{"login": rv.by},
					"html_url": "https://github.com/keycloak/keycloak/pull/7#pullrequestreview-" + strconv.Itoa(i+1)})
			}
			// A pending review is always newer than the submitted ones.
			if g.pendingBy != "" && logins(r) == g.pendingBy {
				out = append(out, map[string]any{"state": "PENDING", "body": "", "user": map[string]string{"login": g.pendingBy},
					"html_url": "https://github.com/keycloak/keycloak/pull/7#pullrequestreview-p"})
			}
			// GitHub lists each reply in a review thread as a review without a body, on the thread's commit.
			for i, rp := range g.replies {
				if rp.thread != 0 {
					out = append(out, map[string]any{"commit_id": g.head, "body": "", "user": map[string]string{"login": rp.by},
						"html_url": "https://github.com/keycloak/keycloak/pull/7#pullrequestreview-r" + strconv.Itoa(i+1)})
				}
			}
		})
		writeJSON(w, 200, page(r, out))
	})
	mux.HandleFunc("POST /markdown", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		_, _ = w.Write([]byte("<p>" + in["text"] + "</p>"))
	})
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		g.guard(func() { g.queries = append(g.queries, r.URL.Query().Get("q")) })
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
	return setupWith(t, func(*server) {})
}

// setupWith lets a test change the server before it starts. By default the bot account is set up
// and alice may post through it.
func setupWith(t *testing.T, configure func(*server)) (*fakeGitHub, *httptest.Server, *http.Client) {
	g := newFakeGitHub(t)
	dir := t.TempDir()
	_ = os.Mkdir(filepath.Join(dir, "reviews"), 0o755)
	e := export{SHA: "abc123", Version: "v1", Intro: "### AI review", By: "Claude Opus 5.5", Answers: []reply{
		{ID: "t55", Thread: 55, Last: 55, Label: "Bob on A.java:10", Body: "**Fixed:**\n\nin thread"},
		{ID: "c9", Label: "Bob in a comment", Body: "Re [Bob in a comment](https://github.com/keycloak/keycloak/pull/7#issuecomment-9):\n\n> point\n\n**Answer:**\n\nin comment"},
		{ID: "c10", Label: "Carol in a comment", Body: "Re [Carol in a comment](https://github.com/keycloak/keycloak/pull/7#issuecomment-10):\n\n> point\n\n**Answer:**\n\nin comment"},
	}, Findings: []finding{
		{ID: 0, Severity: "major", Path: "A.java", Line: 10, InDiff: true, Body: "inline"},
		{ID: 1, Severity: "followup", Path: "B.java", Line: 3, Body: "outside"},
		{ID: 2, Severity: "nit", Path: "A.java", Line: 12, InDiff: true, Body: "nit"},
	}}
	b, _ := json.Marshal(e)
	_ = os.WriteFile(filepath.Join(dir, "reviews", "7.json"), b, 0o644)

	s := &server{clientID: "id", clientSecret: "secret", dataDir: dir, repo: "keycloak/keycloak",
		githubWeb: g.srv.URL, githubAPI: g.srv.URL, client: g.srv.Client(), sessions: map[string]session{},
		bot: bot{token: "bottok", posters: map[string]bool{"alice": true}}}
	configure(s)
	if s.botLogin() == "" && s.bot.token != "" {
		t.Fatal("the bot token didn't work")
	}
	app := httptest.NewTLSServer(s.routes())
	t.Cleanup(app.Close)
	s.publicURL = app.URL
	return g, app, browser(app)
}

// browser is one visitor with their own cookies.
func browser(app *httptest.Server) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Transport: app.Client().Transport, Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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
	return call(t, app, c, "/api/reviews/7", origin, body)
}

func call(t *testing.T, app *httptest.Server, c *http.Client, path, origin, body string) (*http.Response, map[string]string) {
	req, _ := http.NewRequest("POST", app.URL+path, strings.NewReader(body))
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
	res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[0,1]}`)
	if res.StatusCode != 200 || out["url"] == "" {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
	r := g.reviews[0]
	if r.CommitID != "abc123" || r.Event != "COMMENT" || len(r.Comments) != 1 || r.Comments[0].Path != "A.java" || r.Comments[0].Line != 10 {
		t.Fatalf("review %+v", r)
	}
	if !strings.Contains(r.Comments[0].Body, "/reviews/7.html#c-v1-0") {
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
	if res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[0]}`); res.StatusCode != 200 {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
	if r := g.reviews[0]; len(r.Comments) != 0 || !strings.Contains(r.Body, "inline") {
		t.Fatalf("review %+v", r)
	}
}

func TestAPendingReviewIsNoReasonToMoveTheComments(t *testing.T) {
	g, app, c := setup(t)
	g.pending = true
	signIn(t, app, c)
	res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[0]}`)
	want := "GitHub answered 422: Unprocessable Entity. User can only have one pending review per pull request. You have a pending review of this pull request"
	if res.StatusCode != 502 || !strings.HasPrefix(out["error"], want) || g.reviewTries != 1 {
		t.Fatalf("post answered %d %v after %d tries", res.StatusCode, out, g.reviewTries)
	}
}

func TestRefusesToPostInTheOwnNameWhileAReviewIsPending(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	// The person's own review of the commit is on GitHub, listed before the pending one.
	if res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[0]}`); res.StatusCode != 200 {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
	g.pendingBy = g.user
	for _, path := range []string{"/api/reviews/7/preview", "/api/reviews/7"} {
		res, out := call(t, app, c, path, app.URL, `{"version":"v1","ids":[],"answers":["t55"]}`)
		if res.StatusCode != 409 || !strings.Contains(out["error"], "pending review") || len(g.replies) != 0 {
			t.Fatalf("%s answered %d %v, %d replies", path, res.StatusCode, out, len(g.replies))
		}
	}
	// The bot has no pending review, so it posts.
	if res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[],"answers":["t55"],"as":"bot"}`); res.StatusCode != 200 || len(g.replies) != 1 {
		t.Fatalf("post as the bot answered %d %v", res.StatusCode, out)
	}
}

func TestAPendingReviewOfTheBotIsForTheMaintainerToClear(t *testing.T) {
	g, app, c := setup(t)
	g.pendingBy = "tracker-bot"
	signIn(t, app, c)
	res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[0],"as":"bot"}`)
	if res.StatusCode != 409 || !strings.Contains(out["error"], "bot account has a pending review") || len(g.reviews) != 0 {
		t.Fatalf("post as the bot answered %d %v", res.StatusCode, out)
	}
}

func TestWaitsBetweenPostsToGitHub(t *testing.T) {
	g, app, c := setupWith(t, func(s *server) { s.pause = 200 * time.Millisecond })
	signIn(t, app, c)
	start := time.Now()
	if res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[0],"answers":["c9"]}`); res.StatusCode != 200 || len(g.replies) != 1 {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
	// The next post waits for the pause after the last reply of the one before.
	if res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[],"answers":["t55"]}`); res.StatusCode != 200 || len(g.replies) != 2 {
		t.Fatalf("second post answered %d %v", res.StatusCode, out)
	}
	// The review, a reply, and a reply of the next post: two pauses.
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Fatalf("three posts to GitHub took %v", d)
	}
}

func TestRefusesAStaleReview(t *testing.T) {
	for _, tc := range []struct {
		change func(g *fakeGitHub)
		want   string
	}{{func(g *fakeGitHub) { g.head = "def456" }, "next daily run"}, {func(g *fakeGitHub) { g.closed = true }, "is closed"}} {
		g, app, c := setup(t)
		tc.change(g)
		signIn(t, app, c)
		if res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[0]}`); res.StatusCode != 409 || len(g.reviews) != 0 || !strings.Contains(out["error"], tc.want) {
			t.Fatalf("post answered %d %v", res.StatusCode, out)
		}
		if res, out := call(t, app, c, "/api/reviews/7/preview", app.URL, `{"version":"v1","ids":[0]}`); res.StatusCode != 409 || !strings.Contains(out["error"], tc.want) {
			t.Fatalf("preview answered %d %v", res.StatusCode, out)
		}
	}
}

func TestPointsAtTheOAuthAppOnlyWhenGitHubRefusesThePersonsReview(t *testing.T) {
	for _, tc := range []struct {
		message, as string
		hint        bool
	}{{"Forbidden", "", true}, {"You have exceeded a secondary rate limit", "", false}, {"Forbidden", "bot", false}} {
		g, app, c := setup(t)
		g.failReviews, g.failMessage = 403, tc.message
		signIn(t, app, c)
		res, out := post(t, app, c, app.URL, fmt.Sprintf(`{"version":"v1","ids":[0],"as":%q}`, tc.as))
		if res.StatusCode == 200 || !strings.Contains(out["error"], tc.message) || strings.Contains(out["error"], "OAuth App") != tc.hint {
			t.Fatalf("%q as %q: post answered %d %v", tc.message, tc.as, res.StatusCode, out)
		}
	}
}

func TestRefusesOtherOriginsAndUnknownSessions(t *testing.T) {
	g, app, c := setup(t)
	if res, _ := post(t, app, c, app.URL, `{"version":"v1","ids":[0]}`); res.StatusCode != 401 {
		t.Fatalf("without session %d", res.StatusCode)
	}
	signIn(t, app, c)
	if res, _ := post(t, app, c, "https://evil.example", `{"version":"v1","ids":[0]}`); res.StatusCode != 403 || len(g.reviews) != 0 {
		t.Fatalf("other origin %d", res.StatusCode)
	}
}

func TestAcceptsSameSiteFetchWithNullOrigin(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	req, _ := http.NewRequest("POST", app.URL+"/api/reviews/7", strings.NewReader(`{"version":"v1","ids":[1]}`))
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
	req, _ := http.NewRequest("POST", app.URL+"/api/reviews/7/preview", strings.NewReader(`{"version":"v1","ids":[0,1]}`))
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

func TestManyUsersAtOnce(t *testing.T) {
	g, app, _ := setup(t)
	var mu sync.Mutex
	g.lock = &mu
	var wg sync.WaitGroup
	errs := make(chan string, 40)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := browser(app)
			signIn(t, app, c)
			if res, err := c.Get(app.URL + "/api/mine"); err != nil || res.StatusCode != 200 {
				errs <- "mine failed"
			}
			// Everyone signs in as the same person, who reviews the commit once.
			if res, _ := post(t, app, c, app.URL, `{"version":"v1","ids":[1]}`); res.StatusCode != 200 && res.StatusCode != 409 {
				errs <- fmt.Sprintf("post answered %d", res.StatusCode)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if len(g.reviews) != 1 {
		t.Fatalf("%d reviews posted, want 1", len(g.reviews))
	}
}

func me(t *testing.T, app *httptest.Server, c *http.Client) map[string]string {
	res, err := c.Get(app.URL + "/api/me")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("me answered %v %v", res, err)
	}
	var out map[string]string
	_ = json.NewDecoder(res.Body).Decode(&out)
	return out
}

func TestPostsInTheOwnNameByDefault(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	if res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[1]}`); res.StatusCode != 200 {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
	if r := g.reviews[0]; r.by != "alice" || strings.Contains(r.Body, "bot account") {
		t.Fatalf("review by %s: %q", r.by, r.Body)
	}
}

func TestMeNamesTheBotOnlyForPeopleWhoMayUseIt(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	if m := me(t, app, c); m["login"] != "alice" || m["bot"] != "tracker-bot" {
		t.Fatalf("me %v", m)
	}
	g.user = "mallory"
	other := browser(app)
	signIn(t, app, other)
	if m := me(t, app, other); m["login"] != "mallory" || m["bot"] != "" {
		t.Fatalf("me %v", m)
	}
}

func TestPostsThroughTheBotAndNamesWhoAskedForIt(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[0,1],"as":"bot"}`)
	if res.StatusCode != 200 || out["url"] == "" {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
	r := g.reviews[0]
	if r.by != "tracker-bot" || len(r.Comments) != 1 {
		t.Fatalf("review by %s %+v", r.by, r)
	}
	if !strings.Contains(r.Body, "[alice](https://github.com/alice)") || strings.Contains(r.Body, "@alice") {
		t.Fatalf("the body should link alice without mentioning them: %q", r.Body)
	}
	if !strings.HasSuffix(r.Body, "<sub>Automatic review by Claude Opus 5.5: "+app.URL+"/reviews/7.html</sub>") {
		t.Fatalf("the body lost the disclaimer: %q", r.Body)
	}
}

func TestPreviewShowsWhoAskedTheBot(t *testing.T) {
	_, app, c := setup(t)
	signIn(t, app, c)
	req, _ := http.NewRequest("POST", app.URL+"/api/reviews/7/preview", strings.NewReader(`{"version":"v1","ids":[1],"as":"bot"}`))
	req.Header.Set("Origin", app.URL)
	res, err := c.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("preview answered %v %v", res, err)
	}
	var out struct{ Body string }
	_ = json.NewDecoder(res.Body).Decode(&out)
	if !strings.Contains(out.Body, "[alice](https://github.com/alice)") {
		t.Fatalf("preview %q", out.Body)
	}
}

func TestRefusesTheBotToPeopleOutsideTheList(t *testing.T) {
	g, app, c := setup(t)
	g.user = "mallory"
	signIn(t, app, c)
	if res, _ := post(t, app, c, app.URL, `{"version":"v1","ids":[0],"as":"bot"}`); res.StatusCode != 403 || len(g.reviews) != 0 {
		t.Fatalf("post answered %d", res.StatusCode)
	}
	if res, _ := call(t, app, c, "/api/reviews/7/preview", app.URL, `{"version":"v1","ids":[0],"as":"bot"}`); res.StatusCode != 403 {
		t.Fatalf("preview answered %d", res.StatusCode)
	}
}

func TestTheBotPostsEachReviewedCommitOnce(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	if res, _ := post(t, app, c, app.URL, `{"version":"v1","ids":[0],"as":"bot"}`); res.StatusCode != 200 {
		t.Fatalf("first post answered %d", res.StatusCode)
	}
	res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[1],"as":"bot"}`)
	if res.StatusCode != 409 || !strings.Contains(out["earlier"], "pullrequestreview-1") || len(g.reviews) != 1 {
		t.Fatalf("second post answered %d %v", res.StatusCode, out)
	}
	if res, _ := post(t, app, c, app.URL, `{"version":"v1","ids":[1]}`); res.StatusCode != 200 {
		t.Fatalf("a post in the own name answered %d", res.StatusCode)
	}
}

func TestWithoutBotTokenOnlyTheOwnNameIsOffered(t *testing.T) {
	g, app, c := setupWith(t, func(s *server) { s.bot = bot{} })
	signIn(t, app, c)
	if m := me(t, app, c); m["bot"] != "" {
		t.Fatalf("me %v", m)
	}
	if res, _ := post(t, app, c, app.URL, `{"version":"v1","ids":[0],"as":"bot"}`); res.StatusCode != 403 || len(g.reviews) != 0 {
		t.Fatalf("post answered %d", res.StatusCode)
	}
}

func TestRefusesAnUnknownPoster(t *testing.T) {
	_, app, c := setup(t)
	signIn(t, app, c)
	if res, _ := post(t, app, c, app.URL, `{"version":"v1","ids":[0],"as":"someone"}`); res.StatusCode != 400 {
		t.Fatalf("post answered %d", res.StatusCode)
	}
}

func TestParsesThePosterList(t *testing.T) {
	got := posters(" Alice, bob ,,")
	if len(got) != 2 || !got["alice"] || !got["bob"] {
		t.Fatalf("posters %v", got)
	}
}

func TestEveryPostEndsWithTheDisclaimer(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	if res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[0,1],"answers":["t55","c9"]}`); res.StatusCode != 200 {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
	page := app.URL + "/reviews/7.html"
	r := g.reviews[0]
	want := map[string]string{"review": r.Body, "inline comment": r.Comments[0].Body, "thread reply": g.replies[0].body, "comment": g.replies[1].body}
	ends := map[string]string{"review": page, "inline comment": page + "#c-v1-0", "thread reply": page + "#answer-t55", "comment": page + "#answer-c9"}
	for what, body := range want {
		if !strings.HasSuffix(body, "\n\n<sub>Automatic review by Claude Opus 5.5: "+ends[what]+"</sub>") {
			t.Errorf("%s without the disclaimer: %q", what, body)
		}
	}
}

func TestRepliesInThreadsAndCommentsOnThePullRequest(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	if res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[],"answers":["c9","t55","c9"],"as":"bot"}`); res.StatusCode != 200 {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
	if len(g.replies) != 2 {
		t.Fatalf("replies %+v", g.replies)
	}
	if r := g.replies[0]; r.thread != 0 || r.by != "tracker-bot" || !strings.HasPrefix(r.body, "Re [Bob in a comment](") {
		t.Fatalf("comment %+v", r)
	}
	if r := g.replies[1]; r.thread != 55 || r.by != "tracker-bot" || !strings.HasPrefix(r.body, "**Fixed:**\n\nin thread") {
		t.Fatalf("thread reply %+v", r)
	}
	if !strings.Contains(g.replies[1].body, "[alice](https://github.com/alice) asked") {
		t.Fatalf("a reply through the bot should name who asked for it: %q", g.replies[1].body)
	}
}

func TestPostsNoRepliesWithoutPicks(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	if res, _ := post(t, app, c, app.URL, `{"version":"v1","ids":[1]}`); res.StatusCode != 200 || len(g.replies) != 0 {
		t.Fatalf("post answered %d with replies %+v", res.StatusCode, g.replies)
	}
}

func TestRefusesAnUnknownAnswer(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	if res, _ := post(t, app, c, app.URL, `{"version":"v1","ids":[1],"answers":["t99"]}`); res.StatusCode != 400 || len(g.reviews) != 0 {
		t.Fatalf("post answered %d", res.StatusCode)
	}
}

func TestSaysWhichRepliesFailedAfterTheReview(t *testing.T) {
	g, app, c := setup(t)
	g.failReplies = 500
	signIn(t, app, c)
	res, out := postAny(t, app, c, `{"version":"v1","ids":[1],"answers":["t55","c9"]}`)
	failed, _ := out["failed"].([]any)
	if res.StatusCode != 200 || out["url"] == "" || len(failed) != 2 || failed[0] != "Bob on A.java:10" || len(g.reviews) != 1 ||
		!strings.Contains(out["refused"].(string), "Server Error") {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
}

func TestStopsReplyingWhenGitHubRefusesAReply(t *testing.T) {
	for _, tc := range []struct {
		code  int
		tries int32
	}{{403, 1}, {429, 1}, {500, 2}} {
		g, app, c := setup(t)
		g.failReplies, g.failMessage = tc.code, "You have exceeded a secondary rate limit."
		signIn(t, app, c)
		res, out := postAny(t, app, c, `{"version":"v1","ids":[1],"answers":["t55","c9"]}`)
		failed, _ := out["failed"].([]any)
		if res.StatusCode != 200 || len(failed) != 2 || g.replyTries.Load() != tc.tries {
			t.Fatalf("%d: post answered %d %v after %d replies", tc.code, res.StatusCode, out, g.replyTries.Load())
		}
	}
	// A 403 or 429 also stops the replies when another refusal came first, and a 403 points at the OAuth App.
	for _, tc := range []struct {
		code int
		hint bool
	}{{429, false}, {403, true}} {
		g, app, c := setup(t)
		g.replyStatus = []int{500, tc.code}
		signIn(t, app, c)
		res, out := postAny(t, app, c, `{"version":"v1","ids":[1],"answers":["t55","c9","c10"]}`)
		failed, _ := out["failed"].([]any)
		if res.StatusCode != 200 || len(failed) != 3 || g.replyTries.Load() != 2 || strings.Contains(out["refused"].(string), "OAuth App") != tc.hint {
			t.Fatalf("%d: post answered %d %v after %d replies", tc.code, res.StatusCode, out, g.replyTries.Load())
		}
	}
}

func TestSaysAReplyMayHaveGoneOutWhenGitHubDidntAnswer(t *testing.T) {
	// Also when GitHub refused an earlier reply, and when GitHub failed itself, since it sometimes creates the
	// reply then.
	for _, status := range [][]int{{-1, -1}, {404, -1}, {502, 404}} {
		g, app, c := setup(t)
		g.replyStatus = status
		signIn(t, app, c)
		res, out := postAny(t, app, c, `{"version":"v1","ids":[],"answers":["c9","c10"]}`)
		if res.StatusCode != 502 || !strings.Contains(out["error"].(string), "Post again to see which did") {
			t.Fatalf("%v: post answered %d %v", status, res.StatusCode, out)
		}
	}
}

func TestPointsAtTheOAuthAppWhenRepliesAreRefusedAfterTheReview(t *testing.T) {
	for _, tc := range []struct {
		code int
		hint bool
	}{{403, true}, {500, false}} {
		g, app, c := setup(t)
		g.failReplies = tc.code
		signIn(t, app, c)
		res, out := postAny(t, app, c, `{"version":"v1","ids":[1],"answers":["t55","c9"]}`)
		if res.StatusCode != 200 || strings.Contains(out["refused"].(string), "OAuth App") != tc.hint {
			t.Fatalf("%d: post answered %d %v", tc.code, res.StatusCode, out)
		}
	}
}

// postAny posts and decodes the whole answer, with the list of failed replies.
func postAny(t *testing.T, app *httptest.Server, c *http.Client, body string) (*http.Response, map[string]any) {
	req, _ := http.NewRequest("POST", app.URL+"/api/reviews/7", strings.NewReader(body))
	req.Header.Set("Origin", app.URL)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

func TestTheBotSendsOnlyTheRepliesWhenItReviewedTheCommit(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	if res, _ := post(t, app, c, app.URL, `{"version":"v1","ids":[0],"as":"bot"}`); res.StatusCode != 200 {
		t.Fatalf("first post answered %d", res.StatusCode)
	}
	res, out := postAny(t, app, c, `{"version":"v1","ids":[0],"answers":["t55"],"as":"bot"}`)
	if res.StatusCode != 200 || len(g.reviews) != 1 || len(g.replies) != 1 || out["url"] != "" || out["reviewed"] != false ||
		!strings.Contains(out["earlier"].(string), "pullrequestreview-1") {
		t.Fatalf("second post answered %d %v, %d reviews, %d replies", res.StatusCode, out, len(g.reviews), len(g.replies))
	}
}

func TestAReviewThatAPersonWroteDoesNotBlockTheirPost(t *testing.T) {
	g, app, c := setup(t)
	g.reviews = append(g.reviews, postedReview{reviewRequest{CommitID: g.head, Body: "Looks good to me.", Event: "APPROVE"}, g.user})
	signIn(t, app, c)
	if res, out := postAny(t, app, c, `{"version":"v1","ids":[0]}`); res.StatusCode != 200 || out["reviewed"] != true || len(g.reviews) != 2 {
		t.Fatalf("post answered %d %v, %d reviews", res.StatusCode, out, len(g.reviews))
	}
}

func TestARefusedPostListsTheAnswersThatAreOnGitHub(t *testing.T) {
	g, app, c := setup(t)
	g.issueComments = append(g.issueComments, ghComment{ID: 70, Body: "x\n\n<sub>Automatic review by Claude Opus 5.5: " + app.URL + "/reviews/7.html#answer-c9</sub>", HTMLURL: "https://github.com/c70"})
	signIn(t, app, c)
	res, out := postAny(t, app, c, `{"version":"v1","ids":[],"answers":["c9"]}`)
	skipped, _ := out["skipped"].([]any)
	if res.StatusCode != 409 || len(skipped) != 1 || skipped[0].(map[string]any)["url"] != "https://github.com/c70" || len(g.replies) != 0 {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
}

func TestAFailedReviewListsTheAnswersThatAreOnGitHub(t *testing.T) {
	g, app, c := setup(t)
	g.issueComments = append(g.issueComments, ghComment{ID: 70, Body: "x\n\n<sub>Automatic review by Claude Opus 5.5: " + app.URL + "/reviews/7.html#answer-c9</sub>", HTMLURL: "https://github.com/c70"})
	g.failReviews = 500
	signIn(t, app, c)
	res, out := postAny(t, app, c, `{"version":"v1","ids":[0],"answers":["c9"]}`)
	skipped, _ := out["skipped"].([]any)
	// GitHub sometimes creates the review although it answers with an error of its own.
	if res.StatusCode != 502 || len(skipped) != 1 || skipped[0].(map[string]any)["url"] != "https://github.com/c70" || len(g.replies) != 0 ||
		!strings.Contains(out["error"].(string), "GitHub may have posted the review") {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
}

func TestAReviewByAnOlderBuildCounts(t *testing.T) {
	g, app, c := setup(t)
	old := "Summary\n\n<sub>Posted from the [full review](https://tracker.example/reviews/7.html) on the Keycloak OpenID4VC tracker.</sub>"
	g.reviews = append(g.reviews, postedReview{reviewRequest{CommitID: g.head, Body: old, Event: "COMMENT"}, g.user})
	signIn(t, app, c)
	if res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[0]}`); res.StatusCode != 409 || out["earlier"] == "" || len(g.reviews) != 1 {
		t.Fatalf("post answered %d %v, %d reviews", res.StatusCode, out, len(g.reviews))
	}
}

func TestAPersonReviewsEachCommitOnceInTheirOwnName(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	postAny(t, app, c, `{"version":"v1","ids":[0]}`)
	res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[1]}`)
	if res.StatusCode != 409 || !strings.Contains(out["error"], "You already posted") || len(g.reviews) != 1 {
		t.Fatalf("second post answered %d %v, %d reviews", res.StatusCode, out, len(g.reviews))
	}
	if res, out := postAny(t, app, c, `{"version":"v1","ids":[1],"answers":["t55"]}`); res.StatusCode != 200 || out["reviewed"] != false || len(g.reviews) != 1 || len(g.replies) != 1 {
		t.Fatalf("a post with an answer answered %d %v", res.StatusCode, out)
	}
}

func TestTheBotIsDownOnceGitHubRefusesItsToken(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	g.guard(func() { g.botRevoked = true })
	if res, _ := postAny(t, app, c, `{"version":"v1","ids":[0],"as":"bot"}`); res.StatusCode != 502 {
		t.Fatalf("post with a refused token answered %d", res.StatusCode)
	}
	if got := me(t, app, c); got["bot"] != "" || got["botDown"] == "" {
		t.Fatalf("me answered %v after GitHub refused the bot token", got)
	}
}

func TestAReviewInTheirOwnNameDoesNotBlockTheBot(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	post(t, app, c, app.URL, `{"version":"v1","ids":[0]}`)
	if res, _ := post(t, app, c, app.URL, `{"version":"v1","ids":[0],"as":"bot"}`); res.StatusCode != 200 || len(g.reviews) != 2 {
		t.Fatalf("bot post answered %d", res.StatusCode)
	}
}

func TestPostsNoRepliesWhenTheReviewFails(t *testing.T) {
	g, app, c := setup(t)
	g.head = "def456"
	signIn(t, app, c)
	if res, _ := post(t, app, c, app.URL, `{"version":"v1","ids":[0],"answers":["t55"]}`); res.StatusCode != 409 || len(g.replies) != 0 {
		t.Fatalf("post answered %d with %d replies", res.StatusCode, len(g.replies))
	}
}

func TestSaysWhenTheBotTokenStoppedWorking(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	g.botRevoked = true
	res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[0],"as":"bot"}`)
	if res.StatusCode != 502 || !strings.Contains(out["error"], "The bot token may have expired.") {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
}

func TestPeopleOnTheListHearThatTheBotIsDownWithoutAToken(t *testing.T) {
	g, app, c := setupWith(t, func(s *server) { s.bot.token = "" })
	signIn(t, app, c)
	if m := me(t, app, c); m["botDown"] != botMissing || m["bot"] != "" {
		t.Fatalf("me answered %v", m)
	}
	if res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[0],"as":"bot"}`); res.StatusCode != 503 || len(g.reviews) != 0 {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
}

func TestRefusesPicksFromAnOlderPage(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	res, out := post(t, app, c, app.URL, `{"version":"old","ids":[0]}`)
	if res.StatusCode != 409 || !strings.Contains(out["error"], "Reload the page") || len(g.reviews) != 0 {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
}

func TestRefusesAReviewWithoutTheModelNames(t *testing.T) {
	g, app, c := setupWith(t, func(s *server) {
		b, _ := json.Marshal(export{SHA: "abc123", Version: "v1", Intro: "old"})
		_ = os.WriteFile(filepath.Join(s.dataDir, "reviews", "7.json"), b, 0o644)
	})
	signIn(t, app, c)
	if res, _ := post(t, app, c, app.URL, `{"version":"v1","ids":[]}`); res.StatusCode != 409 || len(g.reviews) != 0 {
		t.Fatalf("post answered %d", res.StatusCode)
	}
}

// review.py finds the tracker's own posts on GitHub with TRACKER_POST, so that it doesn't answer them.
// This reads the pattern from review.py and checks it against everything the service posts.
func TestReviewPyRecognisesEveryPost(t *testing.T) {
	src, err := os.ReadFile("../review.py")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`TRACKER_POST = re\.compile\(((?:\s*r"(?:[^"\\]|\\.)*")+)\)`).FindSubmatch(src)
	if block == nil {
		t.Fatal("review.py has no TRACKER_POST")
	}
	pattern := ""
	for _, m := range regexp.MustCompile(`r"((?:[^"\\]|\\.)*)"`).FindAllSubmatch(block[1], -1) {
		pattern += string(m[1])
	}
	if pattern != trackerFooter.String() {
		t.Fatalf("trackerFooter in main.go differs from TRACKER_POST in review.py:\n%s\n%s", trackerFooter, pattern)
	}
	trackerPost := trackerFooter
	link := regexp.MustCompile(`ANSWER_LINK = re\.compile\(r"((?:[^"\\]|\\.)*)"\)`).FindSubmatch(src)
	if link == nil {
		t.Fatal("review.py has no ANSWER_LINK")
	}
	answerLink := regexp.MustCompile(string(link[1]))
	for _, asker := range []string{"", "alice"} {
		s := &server{publicURL: "https://tracker.example", repo: "keycloak/keycloak"}
		e := export{SHA: "abc123", Intro: "intro", By: "Claude Opus 5.5, checked by Claude Fable 5.2",
			Findings: []finding{{ID: 0, Path: "A.java", Line: 1, InDiff: true, Body: "inline"}, {ID: 1, Body: "body"}},
			Answers:  []reply{{ID: "t5", Thread: 5, Body: "thread"}, {ID: "c6", Body: "comment"}}}
		out, err := s.build(7, e, []int{0, 1}, []string{"t5", "c6"}, asker, true)
		if err != nil {
			t.Fatal(err)
		}
		if m := answerLink.FindStringSubmatch(out.replies[1].Body); m == nil || m[1] != "c6" {
			t.Errorf("review.py wouldn't see this post as the answer to c6: %q", out.replies[1].Body)
		}
		bodies := []string{out.review.Body, out.review.Comments[0].Body, out.replies[0].Body, out.replies[1].Body}
		for _, b := range bodies {
			if !trackerPost.MatchString(b) {
				t.Errorf("review.py would answer this post as a person's comment: %q", b)
			}
		}
		// A person who quotes a post in a reply wrote a comment that needs an answer.
		quote := "> " + strings.ReplaceAll(out.replies[0].Body, "\n", "\n> ")
		for _, quoted := range []string{quote + "\n\nNo, it isn't.", "No, it isn't:\n\n" + quote + "\n"} {
			if trackerPost.MatchString(quoted) {
				t.Errorf("review.py would take a quoted post for the tracker's own: %q", quoted)
			}
		}
	}
}

func TestPreviewShowsTheReplies(t *testing.T) {
	_, app, c := setup(t)
	signIn(t, app, c)
	req, _ := http.NewRequest("POST", app.URL+"/api/reviews/7/preview", strings.NewReader(`{"version":"v1","ids":[],"answers":["t55"]}`))
	req.Header.Set("Origin", app.URL)
	res, err := c.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("preview answered %v %v", res, err)
	}
	var out struct {
		Replies []struct{ Label, HTML string }
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	if len(out.Replies) != 1 || out.Replies[0].Label != "Bob on A.java:10" || !strings.Contains(out.Replies[0].HTML, "in thread") {
		t.Fatalf("preview %+v", out)
	}
}

func TestSkipsAnswersThatAreAlreadyOnGitHub(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	postAny(t, app, c, `{"version":"v1","ids":[],"answers":["t55","c9"],"as":"bot"}`)
	res, out := postAny(t, app, c, `{"version":"v1","ids":[0],"answers":["t55","c9"]}`)
	skipped, _ := out["skipped"].([]any)
	if res.StatusCode != 200 || len(g.replies) != 2 || len(skipped) != 2 || len(g.reviews) != 1 {
		t.Fatalf("second post answered %d %v with %d replies", res.StatusCode, out, len(g.replies))
	}
	first := skipped[0].(map[string]any)
	if first["reason"] != "It is already on GitHub." || first["url"] != "https://github.com/reply1" {
		t.Fatalf("skipped %v", first)
	}
}

func TestTheBotRefusesWhenNothingIsLeftToPost(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	postAny(t, app, c, `{"version":"v1","ids":[0],"answers":["t55"],"as":"bot"}`)
	res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[0],"answers":["t55"],"as":"bot"}`)
	if res.StatusCode != 409 || !strings.Contains(out["earlier"], "pullrequestreview-1") || len(g.replies) != 1 {
		t.Fatalf("second post answered %d %v with %d replies", res.StatusCode, out, len(g.replies))
	}
}

func TestSkipsAThreadAnswerWhenSomeoneRepliedSinceTheReview(t *testing.T) {
	g, app, c := setup(t)
	g.threadComments = append(g.threadComments, ghComment{ID: 60, InReplyTo: 55, Body: "Not fixed.", HTMLURL: "https://github.com/c60"})
	signIn(t, app, c)
	res, out := postAny(t, app, c, `{"version":"v1","ids":[0],"answers":["t55","c9"]}`)
	skipped, _ := out["skipped"].([]any)
	if res.StatusCode != 200 || len(g.reviews) != 1 || len(g.replies) != 1 || g.replies[0].thread != 0 || len(skipped) != 1 {
		t.Fatalf("post answered %d %v with replies %v", res.StatusCode, out, g.replies)
	}
	if first := skipped[0].(map[string]any); !strings.Contains(first["reason"].(string), "replied") || first["url"] != "https://github.com/c60" {
		t.Fatalf("skipped %v", first)
	}
}

func TestABotReplyOvertakesAThreadAnswerOnlyAfterATrackerPost(t *testing.T) {
	bot := ghComment{ID: 60, InReplyTo: 55, Body: "Copilot says hi.", HTMLURL: "https://github.com/c60"}
	bot.User.Type = "Bot"
	// The review saw the tracker post, so the answer's last comment is 58.
	tracker := ghComment{ID: 58, InReplyTo: 55, Body: "Fixed.\n\n<sub>Automatic review by Claude Opus 5.5: https://tracker.example/reviews/7.html#answer-t40</sub>"}
	tracker.User.Type = "Bot"
	for _, tc := range []struct {
		name     string
		comments []ghComment
		due      bool
	}{
		{"bot reply only", []ghComment{bot}, true},
		{"bot reply after a tracker post", []ghComment{tracker, bot}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, app, c := setupWith(t, func(s *server) {
				path := filepath.Join(s.dataDir, "reviews", "7.json")
				var e export
				b, _ := os.ReadFile(path)
				_ = json.Unmarshal(b, &e)
				e.Answers[0].Last = 58
				b, _ = json.Marshal(e)
				_ = os.WriteFile(path, b, 0o644)
			})
			g.threadComments = append(g.threadComments, tc.comments...)
			signIn(t, app, c)
			res, out := postAny(t, app, c, `{"version":"v1","ids":[],"answers":["t55"]}`)
			if due := res.StatusCode == 200 && len(g.replies) == 1; due != tc.due {
				t.Fatalf("post answered %d %v, %d replies", res.StatusCode, out, len(g.replies))
			}
			if skipped, _ := out["skipped"].([]any); !tc.due && (len(skipped) != 1 || skipped[0].(map[string]any)["url"] != "https://github.com/c60") {
				t.Fatalf("the bot reply didn't overtake the answer: %v", out)
			}
		})
	}
}

func TestATrackerPostInTheThreadDoesNotOvertakeTheAnswer(t *testing.T) {
	g, app, c := setup(t)
	old := "x\n\n<sub>Claude Code wrote this comment in its [review on the Keycloak OpenID4VC tracker](https://tracker.example/reviews/7.html#c1).</sub>"
	g.threadComments = append(g.threadComments, ghComment{ID: 70, InReplyTo: 55, Body: old, HTMLURL: "https://github.com/c70"})
	signIn(t, app, c)
	if res, out := postAny(t, app, c, `{"version":"v1","ids":[],"answers":["t55"]}`); res.StatusCode != 200 || len(g.replies) != 1 {
		t.Fatalf("post answered %d %v, %d replies", res.StatusCode, out, len(g.replies))
	}
}

func TestFindsAnAnswerOnALaterPageOfComments(t *testing.T) {
	g, app, c := setup(t)
	for i := range 150 {
		g.threadComments = append(g.threadComments, ghComment{ID: int64(1000 + i), Body: "other thread"})
	}
	g.threadComments = append(g.threadComments, ghComment{ID: 2000, InReplyTo: 55, HTMLURL: "https://github.com/c2000",
		Body: "Fixed.\n\n<sub>Automatic review by Claude Opus 5.5: " + app.URL + "/reviews/7.html#answer-t55</sub>"})
	signIn(t, app, c)
	res, out := postAny(t, app, c, `{"version":"v1","ids":[],"answers":["t55"]}`)
	skipped, _ := out["skipped"].([]any)
	if res.StatusCode != 409 || len(skipped) != 1 || skipped[0].(map[string]any)["kind"] != "posted" || len(g.replies) != 0 {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
}

func TestPreviewShowsOnlyTheRepliesWhenTheBotReviewedTheCommit(t *testing.T) {
	_, app, c := setup(t)
	signIn(t, app, c)
	postAny(t, app, c, `{"version":"v1","ids":[0],"as":"bot"}`)
	res, out := previewAny(t, app, c, `{"version":"v1","ids":[0],"answers":["t55"],"as":"bot"}`)
	replies, _ := out["replies"].([]any)
	comments, _ := out["comments"].([]any)
	if res.StatusCode != 200 || out["note"] == "" || out["body"] != "" || len(comments) != 0 || len(replies) != 1 || out["earlier"] == "" {
		t.Fatalf("preview answered %d %v", res.StatusCode, out)
	}
}

func TestPreviewListsAnswersThatDontGoOut(t *testing.T) {
	g, app, c := setup(t)
	g.issueComments = append(g.issueComments, ghComment{ID: 70, Body: "x\n\n<sub>Automatic review by Claude Opus 5.5: " + app.URL + "/reviews/7.html#answer-c9</sub>", HTMLURL: "https://github.com/c70"})
	signIn(t, app, c)
	_, out := previewAny(t, app, c, `{"version":"v1","ids":[],"answers":["t55","c9"]}`)
	replies, _ := out["replies"].([]any)
	skipped, _ := out["skipped"].([]any)
	if len(replies) != 1 || len(skipped) != 1 || skipped[0].(map[string]any)["url"] != "https://github.com/c70" {
		t.Fatalf("preview answered %v", out)
	}
}

// previewAny asks for a preview and decodes the whole answer.
func previewAny(t *testing.T, app *httptest.Server, c *http.Client, body string) (*http.Response, map[string]any) {
	req, _ := http.NewRequest("POST", app.URL+"/api/reviews/7/preview", strings.NewReader(body))
	req.Header.Set("Origin", app.URL)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

func TestTheBotPostsOneReviewWhenPeoplePostAtOnce(t *testing.T) {
	g, app, c := setup(t)
	g.lock = &sync.Mutex{}
	signIn(t, app, c)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			postAny(t, app, c, `{"version":"v1","ids":[0],"as":"bot"}`)
		}()
	}
	wg.Wait()
	if len(g.reviews) != 1 {
		t.Fatalf("the bot posted %d reviews", len(g.reviews))
	}
}

func TestTriesTheBotTokenAgainAfterAFailedStart(t *testing.T) {
	var srv *server
	_, app, c := setupWith(t, func(s *server) { srv = s })
	srv.bot.login = ""
	srv.botTried = time.Now()
	signIn(t, app, c)
	if got := me(t, app, c)["bot"]; got != "" {
		t.Fatalf("me named the bot %q before the retry", got)
	}
	srv.botInit.Lock()
	srv.botTried = time.Time{}
	srv.botInit.Unlock()
	if got := me(t, app, c)["bot"]; got != "tracker-bot" {
		t.Fatalf("me named the bot %q after the retry", got)
	}
}

func TestAPostWithOnlyAnswersSendsNoReview(t *testing.T) {
	g, app, c := setup(t)
	signIn(t, app, c)
	_, view := previewAny(t, app, c, `{"version":"v1","ids":[],"answers":["t55"],"as":"bot"}`)
	if view["body"] != "" || view["note"] == "" {
		t.Fatalf("preview answered %v", view)
	}
	res, out := postAny(t, app, c, `{"version":"v1","ids":[],"answers":["t55"],"as":"bot"}`)
	if res.StatusCode != 200 || len(g.reviews) != 0 || len(g.replies) != 1 || out["url"] != "" || out["reviewed"] != false {
		t.Fatalf("post answered %d %v with %d reviews", res.StatusCode, out, len(g.reviews))
	}
	// The bot's review of the commit is still unused, so the findings can follow.
	if res, _ := postAny(t, app, c, `{"version":"v1","ids":[0],"as":"bot"}`); res.StatusCode != 200 || len(g.reviews) != 1 {
		t.Fatalf("the findings answered %d with %d reviews", res.StatusCode, len(g.reviews))
	}
}

func TestAQuoteOfAnAnswerDoesNotCountAsTheAnswer(t *testing.T) {
	sign := "https://tracker.example/reviews/7.html#answer-c9</sub>"
	post := "**Answered:** yes\n\n<sub>Automatic review by Claude Opus 5.5: " + sign
	if !signed(post+"\n", sign) {
		t.Fatal("the post itself doesn't count")
	}
	if quote := "I disagree:\n\n> " + strings.ReplaceAll(post, "\n", "\n> "); signed(quote, sign) {
		t.Fatalf("a quote that ends the comment counts: %q", quote)
	}
}

func TestSaysWhenTheBotIsDownForPeopleOnTheList(t *testing.T) {
	var srv *server
	g, app, c := setupWith(t, func(s *server) { srv = s })
	srv.bot.login = ""
	srv.botTried = time.Now()
	signIn(t, app, c)
	if got := me(t, app, c); got["bot"] != "" || got["botDown"] == "" {
		t.Fatalf("me answered %v", got)
	}
	res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[0],"as":"bot"}`)
	if res.StatusCode != 503 || out["error"] != botDown || len(g.reviews) != 0 {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
}

func TestSaysWhenNothingWentOut(t *testing.T) {
	for _, tc := range []struct {
		code    int
		message string
		hint    bool
	}{{403, "Forbidden", true}, {422, "Validation Failed", false}, {403, "You have exceeded a secondary rate limit", false}, {404, "Not Found", false}} {
		g, app, c := setup(t)
		g.failReplies, g.failMessage = tc.code, tc.message
		signIn(t, app, c)
		res, out := post(t, app, c, app.URL, `{"version":"v1","ids":[],"answers":["t55"]}`)
		want := fmt.Sprintf("Nothing was posted. GitHub answered %d: %s.", tc.code, tc.message)
		if res.StatusCode != 502 || !strings.Contains(out["error"], want) || strings.Contains(out["error"], "OAuth App") != tc.hint {
			t.Fatalf("post with replies refused by %d answered %d %v", tc.code, res.StatusCode, out)
		}
	}
}

func TestAnAnswerFromAnOlderReviewNeverGoesIntoItsThread(t *testing.T) {
	g, app, c := setupWith(t, func(s *server) {
		e, _ := s.load(7)
		e.Answers[0].Last = 0
		b, _ := json.Marshal(e)
		_ = os.WriteFile(filepath.Join(s.dataDir, "reviews", "7.json"), b, 0o644)
	})
	signIn(t, app, c)
	res, out := postAny(t, app, c, `{"version":"v1","ids":[0],"answers":["t55"]}`)
	if skipped, _ := out["skipped"].([]any); res.StatusCode != 200 || len(g.replies) != 0 || len(skipped) != 1 {
		t.Fatalf("post answered %d %v", res.StatusCode, out)
	}
}
