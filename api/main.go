// Command api signs people in with GitHub and posts tracker reviews to pull requests, in the person's
// own name or through the tracker's bot account.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const sessionTTL = 8 * time.Hour

var returnPath = regexp.MustCompile(`^/reviews/[0-9]+\.html$`)

type session struct {
	token, login string
	expires      time.Time
	mine         *mine
}

// mine holds the items GitHub search finds for the signed-in person.
type mine struct {
	Involved  []int `json:"involved"`
	Requested []int `json:"requested"`
	at        time.Time
}

const mineTTL = 10 * time.Minute

type server struct {
	clientID, clientSecret string
	publicURL, dataDir     string
	repo                   string
	githubWeb, githubAPI   string
	client                 *http.Client

	bot bot

	mu       sync.Mutex
	sessions map[string]session
	// postMu runs one post at a time, so two posts can't send the same review or answer at once.
	// It is held across all GitHub calls of the post, which is fine for the few posts a day here.
	postMu sync.Mutex
	// pause is the wait between two posts to GitHub. GitHub asks for a second between requests that create
	// content, and its secondary rate limit refuses them otherwise.
	pause    time.Duration
	lastPost time.Time // when create last sent something, guarded by postMu
	// botInit guards the bot's login, which botLogin looks up again when GitHub failed at start.
	botInit  sync.Mutex
	botTried time.Time
}

// bot is the tracker's own GitHub account with a classic token that has the public_repo scope. People on
// its list may post through it, for when the repository's organization hasn't approved the OAuth App.
type bot struct {
	token   string
	login   string          // the token's login, or empty while the bot is off. botLogin looks it up, and a 401 clears it.
	posters map[string]bool // lowercase GitHub logins
}

// posters reads a comma-separated list of GitHub logins.
func posters(list string) map[string]bool {
	out := map[string]bool{}
	for _, l := range strings.Split(list, ",") {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			out[l] = true
		}
	}
	return out
}

// lookup asks GitHub for the login of the bot token.
func (b *bot) lookup(s *server) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	if _, err := s.github(http.MethodGet, "/user", b.token, nil, &user); err != nil {
		return "", fmt.Errorf("the bot token doesn't work: %w", err)
	}
	if user.Login == "" {
		return "", fmt.Errorf("GitHub answered no login for the bot token")
	}
	return user.Login, nil
}

// botLogin is the bot's login, or empty while posting through the bot is off. When the token didn't work,
// it tries again at most once a minute, so a short GitHub outage at start doesn't turn the bot off for good.
// One request asks GitHub, and the others go on without the bot until the answer is in.
func (s *server) botLogin() string {
	s.botInit.Lock()
	login := s.bot.login
	retry := login == "" && s.bot.token != "" && time.Since(s.botTried) > time.Minute
	if retry {
		s.botTried = time.Now()
	}
	s.botInit.Unlock()
	if !retry {
		return login
	}
	found, err := s.bot.lookup(s)
	if err != nil {
		log.Printf("posting through the bot is off: %v", err)
		return ""
	}
	s.botInit.Lock()
	s.bot.login = found
	s.botInit.Unlock()
	return found
}

// onBotList says whether login is on BOT_POSTERS. Whether the bot works right now is up to botLogin.
func (s *server) onBotList(login string) bool {
	return s.bot.posters[strings.ToLower(login)]
}

const botDown = "The tracker's bot account isn't available right now. Reload the page in a minute to try again."

// botMissing replaces botDown when the host has no bot token, so reloading won't help.
const botMissing = "Posting through the tracker's bot account isn't set up here. You can post in your own name instead."

// Finding and export mirror review_export in build.py.
type finding struct {
	ID       int    `json:"id"`
	Severity string `json:"severity"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	InDiff   bool   `json:"inDiff"`
	Body     string `json:"body"`
}

type export struct {
	SHA      string    `json:"sha"`
	Version  string    `json:"version"` // changes with every new review, also of the same commit
	Intro    string    `json:"intro"`
	By       string    `json:"by"` // the models for the disclaimer, such as "Claude Opus 5.5, checked by Claude Fable 5.2"
	Findings []finding `json:"findings"`
	Answers  []reply   `json:"answers"`
}

// reply is an answer to a point that a person raised. Thread is the id of the first comment of a review
// thread, and the answer goes into that thread. Without one it goes into a new comment on the pull request.
type reply struct {
	ID     string `json:"id"` // the ref in reviews.json, such as t123
	Thread int64  `json:"thread"`
	Last   int64  `json:"last"`  // the newest comment in the thread when the review was written, 0 in older reviews
	Label  string `json:"label"` // who raised the point and where, for the preview and errors
	Body   string `json:"body"`
}

// posting is everything that a post sends: one review, then the replies.
type posting struct {
	review  reviewRequest
	replies []reply
}

type reviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

type reviewRequest struct {
	CommitID string          `json:"commit_id"`
	Body     string          `json:"body"`
	Event    string          `json:"event"`
	Comments []reviewComment `json:"comments"`
}

func main() {
	s := &server{
		clientID:     os.Getenv("GITHUB_CLIENT_ID"),
		clientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		publicURL:    strings.TrimRight(os.Getenv("PUBLIC_URL"), "/"),
		dataDir:      env("DATA_DIR", "/data"),
		repo:         env("REPO", "keycloak/keycloak"),
		githubWeb:    "https://github.com",
		githubAPI:    "https://api.github.com",
		client:       &http.Client{Timeout: 20 * time.Second},
		pause:        time.Second,
		sessions:     map[string]session{},
		bot:          bot{token: os.Getenv("GITHUB_BOT_TOKEN"), posters: posters(os.Getenv("BOT_POSTERS"))},
	}
	if s.publicURL == "" {
		log.Fatal("PUBLIC_URL is not set")
	}
	// Sign-in and posting in people's own name keep working when the bot token fails.
	if s.bot.token == "" {
		log.Printf("posting through the bot is off: GITHUB_BOT_TOKEN is not set, so %d people on BOT_POSTERS see it as down", len(s.bot.posters))
	} else if login := s.botLogin(); login != "" && len(s.bot.posters) == 0 {
		log.Printf("BOT_POSTERS is empty, so nobody may post through %s", login)
	} else if login != "" {
		log.Printf("people on the list may post through %s: %d logins", login, len(s.bot.posters))
	}
	srv := &http.Server{Addr: env("ADDR", ":8080"), Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", s.login)
	mux.HandleFunc("GET /auth/callback", s.callback)
	mux.HandleFunc("POST /auth/logout", s.sameOrigin(s.logout))
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("GET /api/mine", s.mine)
	mux.HandleFunc("POST /api/reviews/{n}", s.sameOrigin(s.post))
	mux.HandleFunc("POST /api/reviews/{n}/preview", s.sameOrigin(s.preview))
	return mux
}

func random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// The session cookie is SameSite=Lax, and this check also stops cross-site POSTs from older browsers.
// Under the site's no-referrer policy browsers send Origin: null on same-origin POSTs, so Sec-Fetch-Site counts too.
func (s *server) sameOrigin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Fetch-Site") != "same-origin" && r.Header.Get("Origin") != s.publicURL {
			fail(w, http.StatusForbidden, "The request came from another site.")
			return
		}
		h(w, r)
	}
}

func (s *server) cookie(name, value, path string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: path, MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if s.clientID == "" || s.clientSecret == "" {
		http.Error(w, "GitHub sign-in is not set up on this host.", http.StatusServiceUnavailable)
		return
	}
	back := r.URL.Query().Get("return")
	if !returnPath.MatchString(back) {
		back = "/"
	}
	state := random()
	http.SetCookie(w, s.cookie("oauth_state", state+"|"+back, "/auth/", 600))
	q := url.Values{"client_id": {s.clientID}, "redirect_uri": {s.publicURL + "/auth/callback"}, "scope": {"public_repo"}, "state": {state}}
	http.Redirect(w, r, s.githubWeb+"/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}

func (s *server) callback(w http.ResponseWriter, r *http.Request) {
	var state, back string
	if c, err := r.Cookie("oauth_state"); err == nil {
		state, back, _ = strings.Cut(c.Value, "|")
	}
	http.SetCookie(w, s.cookie("oauth_state", "", "/auth/", -1))
	got := r.URL.Query().Get("state")
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(got)) != 1 || (back != "/" && !returnPath.MatchString(back)) {
		http.Error(w, "The sign-in link expired. Go back and sign in again.", http.StatusBadRequest)
		return
	}
	token, err := s.exchange(r.URL.Query().Get("code"))
	if err != nil {
		log.Printf("token exchange: %v", err)
		http.Error(w, "GitHub sign-in failed. Go back and try again.", http.StatusBadGateway)
		return
	}
	var user struct {
		Login string `json:"login"`
	}
	if _, err := s.github(http.MethodGet, "/user", token, nil, &user); err != nil || user.Login == "" {
		log.Printf("user lookup: %v", err)
		http.Error(w, "GitHub sign-in failed. Go back and try again.", http.StatusBadGateway)
		return
	}
	id := random()
	s.mu.Lock()
	now := time.Now()
	for k, v := range s.sessions {
		if now.After(v.expires) {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = session{token: token, login: user.Login, expires: now.Add(sessionTTL)}
	s.mu.Unlock()
	http.SetCookie(w, s.cookie("sid", id, "/", int(sessionTTL.Seconds())))
	http.Redirect(w, r, back, http.StatusFound)
}

func (s *server) exchange(code string) (string, error) {
	form := url.Values{"client_id": {s.clientID}, "client_secret": {s.clientSecret}, "code": {code}, "redirect_uri": {s.publicURL + "/auth/callback"}}
	req, _ := http.NewRequest(http.MethodPost, s.githubWeb+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("no token: %s", out.Error)
	}
	return out.AccessToken, nil
}

func (s *server) session(r *http.Request) (string, session, bool) {
	c, err := r.Cookie("sid")
	if err != nil {
		return "", session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.sessions[c.Value]
	if !ok || time.Now().After(v.expires) {
		delete(s.sessions, c.Value)
		return "", session{}, false
	}
	return c.Value, v, true
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	_, v, ok := s.session(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "Not signed in.")
		return
	}
	out := map[string]string{"login": v.login}
	// People on the list hear that the bot is down also when its token is missing, so they don't post in their
	// own name without choosing to.
	if s.onBotList(v.login) {
		switch out["bot"] = s.botLogin(); {
		case s.bot.token == "":
			out["botDown"] = botMissing
		case out["bot"] == "":
			out["botDown"] = botDown
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if id, v, ok := s.session(r); ok {
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
		s.revoke(v.token)
	}
	http.SetCookie(w, s.cookie("sid", "", "/", -1))
	w.WriteHeader(http.StatusNoContent)
}

// revoke deletes the token at GitHub, so it stops working before it would expire.
func (s *server) revoke(token string) {
	body, _ := json.Marshal(map[string]string{"access_token": token})
	req, _ := http.NewRequest(http.MethodDelete, s.githubAPI+"/applications/"+s.clientID+"/token", bytes.NewReader(body))
	req.SetBasicAuth(s.clientID, s.clientSecret)
	req.Header.Set("Accept", "application/vnd.github+json")
	if res, err := s.client.Do(req); err != nil {
		log.Printf("revoke: %v", err)
	} else {
		_ = res.Body.Close()
	}
}

// github calls the REST API with the given token and decodes a JSON answer into out.
func (s *server) github(method, path, token string, in, out any) (int, error) {
	var b []byte
	if in != nil {
		var err error
		if b, err = json.Marshal(in); err != nil {
			return 0, err
		}
	}
	req, _ := http.NewRequest(method, s.githubAPI+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 300 {
		var e struct {
			Message string            `json:"message"`
			Errors  []json.RawMessage `json:"errors"` // strings, or objects with a message
		}
		_ = json.NewDecoder(res.Body).Decode(&e)
		msg := e.Message
		for _, raw := range e.Errors {
			var text string
			var obj struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(raw, &text) != nil && json.Unmarshal(raw, &obj) == nil {
				text = obj.Message
			}
			if text != "" {
				msg += ". " + text
			}
		}
		err := fmt.Errorf("GitHub answered %d: %s", res.StatusCode, msg)
		if res.StatusCode == http.StatusUnauthorized && token == s.bot.token && token != "" {
			err = fmt.Errorf("%w. The bot token may have expired.", err)
			// /api/me then says the bot is down, and botLogin tries the token again in a minute.
			s.botInit.Lock()
			s.bot.login, s.botTried = "", time.Now()
			s.botInit.Unlock()
		}
		return res.StatusCode, err
	}
	if out != nil {
		return res.StatusCode, json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode, nil
}

func (s *server) load(n int) (export, error) {
	var e export
	b, err := os.ReadFile(filepath.Join(s.dataDir, "reviews", strconv.Itoa(n)+".json"))
	if err != nil {
		return e, err
	}
	return e, json.Unmarshal(b, &e)
}

// build turns the picked findings into a GitHub review and the picked answers into replies. Findings
// outside the diff, or all of them when inline is false, go into the review body, because GitHub refuses
// inline comments there. asker is the login of the person who posts through the bot, and empty for a post
// in their own name.
func (s *server) build(n int, e export, ids []int, answers []string, asker string, inline bool) (posting, error) {
	page := fmt.Sprintf("%s/reviews/%d.html", s.publicURL, n)
	// Each post ends with the disclaimer, and posts through the bot first name who asked for them.
	// A link to the profile, because @login would notify the person.
	sign := func(text, anchor string) string {
		parts := []string{text}
		if asker != "" {
			parts = append(parts, fmt.Sprintf("<sub>[%s](https://github.com/%s) asked the tracker's bot account to post this.</sub>", asker, asker))
		}
		parts = append(parts, fmt.Sprintf("<sub>Automatic review by %s: %s%s</sub>", e.By, page, anchor))
		return strings.Join(parts, "\n\n")
	}
	byID := map[int]finding{}
	for _, f := range e.Findings {
		byID[f.ID] = f
	}
	out := posting{review: reviewRequest{CommitID: e.SHA, Event: "COMMENT", Comments: []reviewComment{}}}
	body := []string{e.Intro}
	seen := map[int]bool{}
	for _, id := range ids {
		f, ok := byID[id]
		if !ok {
			return out, fmt.Errorf("the review has no comment %d", id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if inline && f.InDiff {
			c := reviewComment{Path: f.Path, Line: f.Line, Side: "RIGHT", Body: sign(f.Body, fmt.Sprintf("#c-%s-%d", e.Version, f.ID))}
			out.review.Comments = append(out.review.Comments, c)
			continue
		}
		where := ""
		if f.Path != "" {
			where = fmt.Sprintf("[`%s:%d`](https://github.com/%s/blob/%s/%s#L%d)\n\n", f.Path, f.Line, s.repo, e.SHA, f.Path, f.Line)
		}
		body = append(body, "---\n\n"+where+f.Body)
	}
	out.review.Body = sign(strings.Join(body, "\n\n"), "")
	byAnswer := map[string]reply{}
	for _, a := range e.Answers {
		byAnswer[a.ID] = a
	}
	picked := map[string]bool{}
	for _, id := range answers {
		a, ok := byAnswer[id]
		if !ok {
			return out, fmt.Errorf("the review has no answer %s", id)
		}
		if picked[id] {
			continue
		}
		picked[id] = true
		a.Body = sign(a.Body, "#answer-"+a.ID)
		out.replies = append(out.replies, a)
	}
	return out, nil
}

// pick is a post or preview request: the review, the picked findings and who posts it.
type pick struct {
	who     session
	n       int
	e       export
	ids     []int
	answers []string
	asker   string // the signed-in login when the bot posts, else empty
	poster  string // the login that posts, the bot's or the person's
	token   string // the token that posts, the person's own or the bot's
	out     posting
}

// picked reads a post or preview request. It answers the error itself and returns ok false then.
func (s *server) picked(w http.ResponseWriter, r *http.Request) (p pick, ok bool) {
	if _, p.who, ok = s.session(r); !ok {
		fail(w, http.StatusUnauthorized, "Your sign-in expired. Reload the page and sign in again.")
		return p, false
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n <= 0 {
		fail(w, http.StatusNotFound, "No such pull request.")
		return p, false
	}
	p.n = n
	var in struct {
		Version string   `json:"version"` // the version of the review on the page
		IDs     []int    `json:"ids"`
		Answers []string `json:"answers"`
		As      string   `json:"as"` // "bot" posts through the bot account, empty posts in their own name
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "The request is malformed.")
		return p, false
	}
	switch in.As {
	case "":
		p.poster, p.token = p.who.login, p.who.token
	case "bot":
		if !s.onBotList(p.who.login) {
			fail(w, http.StatusForbidden, "Only people on the tracker's list may post through its bot account.")
			return p, false
		}
		// Read once, because a refused bot token clears it meanwhile.
		if p.poster = s.botLogin(); p.poster == "" && s.bot.token == "" {
			fail(w, http.StatusServiceUnavailable, botMissing)
			return p, false
		} else if p.poster == "" {
			fail(w, http.StatusServiceUnavailable, botDown)
			return p, false
		}
		p.asker, p.token = p.who.login, s.bot.token
	default:
		fail(w, http.StatusBadRequest, "The request is malformed.")
		return p, false
	}
	p.ids, p.answers = in.IDs, in.Answers
	if p.e, err = s.load(n); err != nil {
		fail(w, http.StatusNotFound, "This pull request has no review.")
		return p, false
	}
	// Findings are picked by their position, so picks from a page with an older review don't fit this one.
	if in.Version != p.e.Version {
		fail(w, http.StatusConflict, "The review on this page changed. Reload the page and pick again.")
		return p, false
	}
	if p.e.By == "" {
		fail(w, http.StatusConflict, "This review is from an older build of the tracker. Post it after the next daily update.")
		return p, false
	}
	if p.out, err = s.build(n, p.e, p.ids, p.answers, p.asker, true); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return p, false
	}
	return p, true
}

func (s *server) post(w http.ResponseWriter, r *http.Request) {
	p, ok := s.picked(w, r)
	if !ok {
		return
	}
	s.postMu.Lock()
	defer s.postMu.Unlock()
	if !s.current(w, p) {
		return
	}
	as := p.poster
	if p.asker != "" {
		as += " for " + p.asker
	}
	earlier, skipped, err := s.check(&p)
	if err != nil {
		failCheck(w, p, err)
		return
	}
	reviewURL := ""
	only := repliesOnly(p, earlier)
	if only != "" && len(p.out.replies) == 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": only, "earlier": earlier, "skipped": skipped})
		return
	}
	if only == "" {
		var ok bool
		if reviewURL, ok = s.postReview(w, p, as, skipped); !ok {
			return
		}
	}
	// When the review is on GitHub, the post succeeded and the answer is 200 even when replies fail.
	// failed and refused say which replies to post again.
	reviewed := only == ""
	failed := []string{}
	var refused error             // the first reply GitHub refused
	denied := false               // whether GitHub refused a reply as a refusal of access
	stop := false                 // whether GitHub answered a reply with 403 or 429
	unsure := false               // whether GitHub may have created a reply that it didn't confirm
	posted := map[string]string{} // answer id to the link of its reply
	for i, a := range p.out.replies {
		// After a 403 or 429, GitHub would refuse the other replies too, and more tries prolong a rate limit.
		if stop {
			for _, rest := range p.out.replies[i:] {
				failed = append(failed, rest.Label)
			}
			break
		}
		path := fmt.Sprintf("/repos/%s/issues/%d/comments", s.repo, p.n)
		if a.Thread != 0 {
			path = fmt.Sprintf("/repos/%s/pulls/%d/comments/%d/replies", s.repo, p.n, a.Thread)
		}
		var out struct {
			HTMLURL string `json:"html_url"`
		}
		if code, err := s.create(path, p.token, map[string]string{"body": a.Body}, &out); err != nil {
			log.Printf("reply #%d to %s as %s: %v", p.n, a.ID, as, err)
			failed = append(failed, a.Label)
			if refused == nil {
				refused = err
			}
			stop = code == http.StatusForbidden || code == http.StatusTooManyRequests
			unsure = unsure || mayHavePosted(code)
			denied = denied || accessRefused(code, err)
		} else {
			posted[a.ID] = out.HTMLURL
		}
	}
	if len(p.out.replies) > 0 {
		log.Printf("replied on #%d as %s to %d of %d answers", p.n, as, len(p.out.replies)-len(failed), len(p.out.replies))
	}
	if !reviewed && len(posted) == 0 {
		msg := fmt.Sprintf("GitHub refused the replies. Nothing was posted. %s.", strings.TrimSuffix(refused.Error(), "."))
		if unsure {
			// Posting again finds a reply that went out and leaves it out.
			msg = fmt.Sprintf("GitHub didn't confirm every reply, so some may have gone out. Post again to see which did. %s.", strings.TrimSuffix(refused.Error(), "."))
		}
		// As for reviews, only a refusal of access points at the OAuth App.
		if p.asker == "" && denied {
			msg += s.oauthHint()
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": msg, "earlier": earlier, "skipped": skipped})
		return
	}
	// url is the new review. earlier is the review of this commit that was already on GitHub.
	// refused is GitHub's reason for the first reply it refused.
	reason := ""
	if refused != nil {
		reason = refused.Error()
		if p.asker == "" && denied {
			reason = strings.TrimSuffix(reason, ".") + "." + s.oauthHint()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": reviewURL, "earlier": earlier, "reviewed": reviewed, "failed": failed, "refused": reason, "skipped": skipped, "posted": posted})
}

// repliesOnly says why a post sends only the picked answers and no review, or is empty when it sends a review.
// Each account reviews a commit once, so a post with answers but no findings doesn't use up that review.
// A post with neither findings nor answers sends the summary as the review. It runs after check, so
// p.out.replies holds the answers that are still due.
func repliesOnly(p pick, earlier string) string {
	why := ""
	switch {
	case earlier != "" && p.asker != "":
		why = "The bot account already posted a review of this commit"
	case earlier != "":
		why = "You already posted a review of this commit"
	case len(p.ids) == 0 && len(p.answers) > 0:
		why = "No findings are picked"
	default:
		return ""
	}
	switch {
	case len(p.out.replies) > 0:
		return why + ", so only the replies go out."
	case len(p.answers) == 0 && len(p.e.Answers) == 0:
		return why + "."
	case len(p.answers) == 0:
		return why + ". Only answers can still go out as this account."
	}
	return why + ", and none of the picked answers is still due."
}

// skip is a picked answer that doesn't go out, with the reason and a link to what is on GitHub instead.
type skip struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // "posted" or "overtaken"
	Label  string `json:"label"`
	Reason string `json:"reason"`
	URL    string `json:"url"`
}

// check looks at GitHub before a post. It returns the earlier review of this commit by the account that
// posts, if any. It drops the picked answers that are no longer due from p.out.replies and returns them: answers
// that are already on GitHub, and thread answers to which someone replied since the review was written.
// The tracker's posts end with a link to the answer on the review page, which is how it finds its own.
func (s *server) check(p *pick) (earlier string, skipped []skip, err error) {
	if earlier, err = s.reviewBy(p.n, p.e.SHA, p.poster, p.token); err != nil {
		return "", nil, err
	}
	var threads, comments []ghComment
	due := p.out.replies[:0:0]
	for _, a := range p.out.replies {
		// Without the host, so posts from before a change of PUBLIC_URL count too, as in review.py.
		sign := fmt.Sprintf("/reviews/%d.html#answer-%s</sub>", p.n, a.ID)
		var list []ghComment
		if a.Thread != 0 {
			if threads == nil {
				if threads, err = s.comments(p.token, fmt.Sprintf("/repos/%s/pulls/%d/comments", s.repo, p.n)); err != nil {
					return "", nil, err
				}
			}
			for _, c := range threads {
				// Replies point at the first comment of their thread.
				if c.ID == a.Thread || c.InReplyTo == a.Thread {
					list = append(list, c)
				}
			}
		} else {
			if comments == nil {
				if comments, err = s.comments(p.token, fmt.Sprintf("/repos/%s/issues/%d/comments", s.repo, p.n)); err != nil {
					return "", nil, err
				}
			}
			list = comments
		}
		var newest ghComment
		posted := ""
		tracker := false // whether the thread has a tracker post before c
		for _, c := range list {
			if c.ID > a.Last && signed(c.Body, sign) {
				posted = c.HTMLURL
			}
			// As in review.py, the tracker's own posts need no answer, and a bot's comment needs one only after
			// a tracker post in its thread. Comments that need no answer don't overtake the answer either.
			// A reply by the pull request's author does, also where review.py wouldn't answer it, because it may
			// settle the point. The answer then waits for the next review, which sees the reply.
			mine := trackerPost(c.Body)
			if c.ID > newest.ID && !mine && (c.User.Type != "Bot" || tracker) {
				newest = c
			}
			tracker = tracker || mine
		}
		switch {
		case posted != "":
			skipped = append(skipped, skip{a.ID, "posted", a.Label, "It is already on GitHub.", posted})
		// Older reviews don't know the newest comment (Last is 0), so their thread answers never go out.
		// Comment ids grow in the order GitHub creates the comments. A reply drafted in a pending review gets
		// its id early, so if it is submitted after the review, it doesn't overtake the answer here.
		// GitHub's REST API doesn't say whether a comment is hidden, so a hidden reply overtakes the answer,
		// although review.py doesn't answer it.
		case a.Thread != 0 && newest.ID > a.Last:
			skipped = append(skipped, skip{a.ID, "overtaken", a.Label, "Someone replied in the thread after this review. A newer review of this pull request may answer that too.", newest.HTMLURL})
		default:
			due = append(due, a)
		}
	}
	p.out.replies = due
	return earlier, skipped, nil
}

// signed says whether body ends with the disclaimer line sign, as the posts of this service do.
// A quote of a post ends with "> <sub>...", so it doesn't count.
func signed(body, sign string) bool {
	body = strings.TrimSpace(body)
	last := body[strings.LastIndex(body, "\n")+1:]
	return strings.HasPrefix(last, "<sub>") && strings.HasSuffix(last, sign)
}

// trackerFooter is TRACKER_POST from review.py, so that both find the same posts of this tracker, from this
// build and older ones: the last line is the disclaimer with a link to a review page.
// TestReviewPyRecognisesEveryPost checks that the two patterns are the same.
var trackerFooter = regexp.MustCompile(`(?:^|\n)<sub>(Automatic review by [^<\n]+: \S+/reviews/\d+\.html(#[\w-]+)?|Posted from the \[full review\]\([^)]*/reviews/\d+\.html\) on the Keycloak OpenID4VC tracker\.|Claude Code wrote this comment in its \[review on the Keycloak OpenID4VC tracker\]\([^)]*/reviews/\d+\.html#c\d+\)\.)</sub>\s*$`)

func trackerPost(body string) bool {
	return trackerFooter.MatchString(body)
}

// ghComment is a comment from GitHub's REST API, oldest first. InReplyTo is set on replies in review threads.
type ghComment struct {
	ID        int64  `json:"id"`
	InReplyTo int64  `json:"in_reply_to_id"`
	Body      string `json:"body"`
	HTMLURL   string `json:"html_url"`
	User      struct {
		Type string `json:"type"` // "Bot" for GitHub Apps such as Copilot
	} `json:"user"`
}

// comments lists the comments at path, the oldest first and up to 1000. On a longer pull request it misses
// the newest ones, so an answer may go out twice there or miss a newer reply.
func (s *server) comments(token, path string) ([]ghComment, error) {
	out := []ghComment{}
	for page := 1; page <= 10; page++ {
		var cs []ghComment
		if _, err := s.github(http.MethodGet, fmt.Sprintf("%s?per_page=100&page=%d", path, page), token, nil, &cs); err != nil {
			return nil, err
		}
		out = append(out, cs...)
		if len(cs) < 100 {
			break
		}
	}
	return out, nil
}

// create sends a POST that creates content on GitHub, a pause after the one before. Callers hold postMu.
func (s *server) create(path, token string, body, out any) (int, error) {
	time.Sleep(time.Until(s.lastPost.Add(s.pause)))
	code, err := s.github(http.MethodPost, path, token, body, out)
	s.lastPost = time.Now()
	return code, err
}

// accessRefused says whether GitHub refused access, which for a person's own name may be the organization's
// restriction of the OAuth App. GitHub's rate limit answers 403 too, but has nothing to do with it, and a 404
// more likely means that the comment to reply to is gone.
func accessRefused(code int, err error) bool {
	return code == http.StatusForbidden && !strings.Contains(strings.ToLower(err.Error()), "rate limit")
}

// oauthHint follows GitHub's 403 answers to posts in a person's own name.
func (s *server) oauthHint() string {
	owner, _, _ := strings.Cut(s.repo, "/")
	return fmt.Sprintf(" The %s organization may need to approve this OAuth App first.", owner)
}

// postReview posts the review and returns its link. It answers the error itself and returns ok false then.
// The error lists the skipped answers, so the page marks them also when the review fails.
func (s *server) postReview(w http.ResponseWriter, p pick, as string, skipped []skip) (string, bool) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("/repos/%s/pulls/%d/reviews", s.repo, p.n)
	req := p.out.review
	code, err := s.create(path, p.token, req, &out)
	// GitHub keeps one pending review per person and pull request, and refuses a new review meanwhile.
	pending := code == http.StatusUnprocessableEntity && strings.Contains(err.Error(), "pending review")
	if code == http.StatusUnprocessableEntity && !pending && len(req.Comments) > 0 {
		// A line that GitHub doesn't count as part of the diff fails the whole review.
		fallback, _ := s.build(p.n, p.e, p.ids, p.answers, p.asker, false)
		req = fallback.review
		code, err = s.create(path, p.token, req, &out)
	}
	if err != nil {
		log.Printf("post #%d as %s: %v", p.n, as, err)
		msg := err.Error()
		switch {
		case pending:
			msg += ". " + pendingHint(p)
		case p.asker == "" && accessRefused(code, err):
			msg = strings.TrimSuffix(msg, ".") + "." + s.oauthHint()
		case mayHavePosted(code):
			// Posting again finds the review if it went out, and then sends only the answers.
			msg = "GitHub may have posted the review. Post again to see whether it did. " + strings.TrimSuffix(msg, ".") + "."
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": msg, "skipped": skipped})
		return "", false
	}
	log.Printf("posted #%d as %s with %d inline comments", p.n, as, len(req.Comments))
	return out.HTMLURL, true
}

// mayHavePosted says whether GitHub may have created what a failed request asked for: when it didn't answer,
// when it failed itself (it sometimes answers 502 or 504 after creating the content), or when its answer of
// success couldn't be read.
func mayHavePosted(code int) bool {
	return code == 0 || code >= 500 || code < 300
}

// failCheck answers a failed check: a pending review is a conflict, anything else is GitHub's failure.
func failCheck(w http.ResponseWriter, p pick, err error) {
	if errors.Is(err, errPending) {
		fail(w, http.StatusConflict, pendingHint(p))
		return
	}
	fail(w, http.StatusBadGateway, err.Error())
}

// errPending is the answer of reviewBy when login has a pending review of the pull request. GitHub refuses a
// new review then, and may add replies to the pending review, where nobody else sees them.
var errPending = errors.New("pending review")

// pendingHint says who has to clear the pending review that blocks a post: the person, or for the bot the
// tracker's maintainer, since only the bot account sees its pending review.
func pendingHint(p pick) string {
	if p.asker != "" {
		return "The tracker's bot account has a pending review of this pull request on GitHub. Ask the tracker's maintainer to submit or delete it."
	}
	return "You have a pending review of this pull request on GitHub. Submit or delete it there, then post again."
}

// reviewBy returns the link to a tracker review that login already posted on the commit, or empty.
// A review that the person wrote themselves doesn't count.
// It answers errPending when login has a pending review, so it doesn't stop at the first match and reads up to
// 10 pages, which is 1,000 reviews.
func (s *server) reviewBy(n int, sha, login, token string) (string, error) {
	earlier := ""
	for page := 1; page <= 10; page++ {
		var reviews []struct {
			State    string `json:"state"`
			CommitID string `json:"commit_id"`
			Body     string `json:"body"`
			HTMLURL  string `json:"html_url"`
			User     struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		path := fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100&page=%d", s.repo, n, page)
		if _, err := s.github(http.MethodGet, path, token, nil, &reviews); err != nil {
			return "", err
		}
		for _, rv := range reviews {
			// GitHub lists a pending review only to its author.
			if rv.State == "PENDING" && strings.EqualFold(rv.User.Login, login) {
				return "", errPending
			}
			// Replies in review threads come as reviews without a body, and a review that a person wrote
			// has no tracker footer, so neither counts.
			// Older builds ended the review with "Posted from the [full review](…) on the Keycloak OpenID4VC tracker."
			tracker := signed(rv.Body, fmt.Sprintf("/reviews/%d.html</sub>", n)) ||
				signed(rv.Body, fmt.Sprintf("/reviews/%d.html) on the Keycloak OpenID4VC tracker.</sub>", n))
			if earlier == "" && rv.CommitID == sha && tracker && strings.EqualFold(rv.User.Login, login) {
				earlier = rv.HTMLURL
			}
		}
		if len(reviews) < 100 {
			break
		}
	}
	return earlier, nil
}

// current says whether the pull request is open and still at the reviewed commit. It answers the error itself.
func (s *server) current(w http.ResponseWriter, p pick) bool {
	var pr struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		State string `json:"state"`
	}
	if _, err := s.github(http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", s.repo, p.n), p.token, nil, &pr); err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return false
	}
	if pr.State != "open" {
		fail(w, http.StatusConflict, "The pull request is closed, so this review can't be posted anymore.")
		return false
	}
	if pr.Head.SHA != p.e.SHA {
		fail(w, http.StatusConflict, "The pull request changed since this review. The next daily run reviews the new commits.")
		return false
	}
	return true
}

// preview renders the review exactly as post would send it, with GitHub's own Markdown renderer.
func (s *server) preview(w http.ResponseWriter, r *http.Request) {
	p, ok := s.picked(w, r)
	if !ok || !s.current(w, p) {
		return
	}
	earlier, skipped, err := s.check(&p)
	if err != nil {
		failCheck(w, p, err)
		return
	}
	req := p.out.review
	note := repliesOnly(p, earlier)
	if note != "" {
		req = reviewRequest{}
	}
	type comment struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		HTML string `json:"html"`
	}
	type answer struct {
		Label  string `json:"label"`
		Thread bool   `json:"thread"`
		HTML   string `json:"html"`
	}
	out := struct {
		Body     string    `json:"body"`
		Comments []comment `json:"comments"`
		Replies  []answer  `json:"replies"`
		Note     string    `json:"note"`
		Earlier  string    `json:"earlier"` // the poster's review of this commit, when there is one
		Skipped  []skip    `json:"skipped"`
	}{Comments: make([]comment, len(req.Comments)), Replies: make([]answer, len(p.out.replies)), Note: note, Earlier: earlier, Skipped: append([]skip{}, skipped...)}
	texts := []string{req.Body}
	for _, c := range req.Comments {
		texts = append(texts, c.Body)
	}
	for _, a := range p.out.replies {
		texts = append(texts, a.Body)
	}
	html := make([]string, len(texts))
	errs := make([]error, len(texts))
	var wg sync.WaitGroup
	for i, t := range texts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if t != "" {
				html[i], errs[i] = s.markdown(t, p.who.token)
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	out.Body = html[0]
	for i, c := range req.Comments {
		out.Comments[i] = comment{Path: c.Path, Line: c.Line, HTML: html[i+1]}
	}
	for i, a := range p.out.replies {
		out.Replies[i] = answer{Label: a.Label, Thread: a.Thread != 0, HTML: html[1+len(req.Comments)+i]}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) markdown(text, token string) (string, error) {
	b, _ := json.Marshal(map[string]string{"text": text, "mode": "gfm", "context": s.repo})
	req, _ := http.NewRequest(http.MethodPost, s.githubAPI+"/markdown", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err == nil && res.StatusCode != http.StatusOK {
		err = fmt.Errorf("GitHub answered %d to the preview", res.StatusCode)
	}
	return string(out), err
}

// mine answers the items that involve the signed-in person, and the open PRs that wait for their
// review. GitHub search counts review requests to teams they belong to, such as CODEOWNERS teams.
func (s *server) mine(w http.ResponseWriter, r *http.Request) {
	id, v, ok := s.session(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "Not signed in.")
		return
	}
	if v.mine != nil && time.Since(v.mine.at) < mineTTL {
		writeJSON(w, http.StatusOK, v.mine)
		return
	}
	m := &mine{at: time.Now()}
	var err error
	if m.Involved, err = s.search(v.token, "involves:"+v.login); err == nil {
		m.Requested, err = s.search(v.token, "is:pr is:open review-requested:"+v.login)
	}
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	s.mu.Lock()
	if cur, ok := s.sessions[id]; ok {
		cur.mine = m
		s.sessions[id] = cur
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, m)
}

// search returns the issue and PR numbers of a GitHub search in the repository, at most 1000.
func (s *server) search(token, q string) ([]int, error) {
	nums := []int{}
	for page := 1; page <= 10; page++ {
		var res struct {
			Items []struct {
				Number int `json:"number"`
			} `json:"items"`
		}
		query := url.Values{"q": {"repo:" + s.repo + " " + q}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if _, err := s.github(http.MethodGet, "/search/issues?"+query.Encode(), token, nil, &res); err != nil {
			return nil, err
		}
		for _, it := range res.Items {
			nums = append(nums, it.Number)
		}
		if len(res.Items) < 100 {
			break
		}
	}
	return nums, nil
}
