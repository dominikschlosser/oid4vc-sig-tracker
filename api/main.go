// Command api signs people in with GitHub and posts tracker reviews to pull requests in their name.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
}

type server struct {
	clientID, clientSecret string
	publicURL, dataDir     string
	repo                   string
	githubWeb, githubAPI   string
	client                 *http.Client

	mu       sync.Mutex
	sessions map[string]session
}

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
	Intro    string    `json:"intro"`
	Findings []finding `json:"findings"`
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
		sessions:     map[string]session{},
	}
	if s.publicURL == "" {
		log.Fatal("PUBLIC_URL is not set")
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
	mux.HandleFunc("POST /api/reviews/{n}", s.sameOrigin(s.post))
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
	writeJSON(w, http.StatusOK, map[string]string{"login": v.login})
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

// github calls the REST API with the user's token and decodes a JSON answer into out.
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
			Message string `json:"message"`
		}
		_ = json.NewDecoder(res.Body).Decode(&e)
		return res.StatusCode, fmt.Errorf("GitHub answered %d: %s", res.StatusCode, e.Message)
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

// build turns the picked findings into a GitHub review. Findings outside the diff, or all of them
// when inline is false, go into the review body, because GitHub refuses inline comments there.
func (s *server) build(n int, e export, ids []int, inline bool) (reviewRequest, error) {
	byID := map[int]finding{}
	for _, f := range e.Findings {
		byID[f.ID] = f
	}
	req := reviewRequest{CommitID: e.SHA, Event: "COMMENT", Comments: []reviewComment{}}
	body := []string{e.Intro}
	seen := map[int]bool{}
	for _, id := range ids {
		f, ok := byID[id]
		if !ok {
			return req, fmt.Errorf("the review has no comment %d", id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if inline && f.InDiff {
			req.Comments = append(req.Comments, reviewComment{Path: f.Path, Line: f.Line, Side: "RIGHT", Body: f.Body})
			continue
		}
		where := ""
		if f.Path != "" {
			where = fmt.Sprintf("[`%s:%d`](https://github.com/%s/blob/%s/%s#L%d)\n\n", f.Path, f.Line, s.repo, e.SHA, f.Path, f.Line)
		}
		body = append(body, "---\n\n"+where+f.Body)
	}
	body = append(body, fmt.Sprintf("<sub>[Full review](%s/reviews/%d.html) on the Keycloak OpenID4VC tracker.</sub>", s.publicURL, n))
	req.Body = strings.Join(body, "\n\n")
	return req, nil
}

func (s *server) post(w http.ResponseWriter, r *http.Request) {
	_, v, ok := s.session(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "Your sign-in expired. Reload the page and sign in again.")
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n <= 0 {
		fail(w, http.StatusNotFound, "No such pull request.")
		return
	}
	var in struct {
		IDs []int `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "The request is malformed.")
		return
	}
	e, err := s.load(n)
	if err != nil {
		fail(w, http.StatusNotFound, "This pull request has no review.")
		return
	}
	var pr struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		State string `json:"state"`
	}
	if _, err := s.github(http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", s.repo, n), v.token, nil, &pr); err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if pr.State != "open" || pr.Head.SHA != e.SHA {
		fail(w, http.StatusConflict, "The pull request changed since this review. The next daily run reviews the new commits.")
		return
	}
	req, err := s.build(n, e, in.IDs, true)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("/repos/%s/pulls/%d/reviews", s.repo, n)
	code, err := s.github(http.MethodPost, path, v.token, req, &out)
	if code == http.StatusUnprocessableEntity && len(req.Comments) > 0 {
		// A line that GitHub doesn't count as part of the diff fails the whole review.
		req, _ = s.build(n, e, in.IDs, false)
		code, err = s.github(http.MethodPost, path, v.token, req, &out)
	}
	if err != nil {
		log.Printf("post #%d as %s: %v", n, v.login, err)
		msg := err.Error()
		if code == http.StatusForbidden || code == http.StatusNotFound {
			msg += " The keycloak organization may need to approve this OAuth App first."
		}
		fail(w, http.StatusBadGateway, msg)
		return
	}
	log.Printf("posted #%d as %s with %d inline comments", n, v.login, len(req.Comments))
	writeJSON(w, http.StatusOK, map[string]string{"url": out.HTMLURL})
}
