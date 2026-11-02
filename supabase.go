package main

// Supabase: email/password accounts and saved solo results.
//
// The browser never talks to Supabase. The server signs players in through
// Supabase Auth, keeps their session in HttpOnly cookies, and saves each
// finished solo game itself, so results can't be faked from the browser.
// Without the SUPABASE_* settings everything here switches off and the game
// works as before.

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/mail"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed login.html
var loginHTML string
var loginPage = template.Must(template.New("login").Parse(loginHTML))

var supa = struct {
	url    string // e.g. https://abcd.supabase.co
	public string // publishable / anon key: allowed for sign-in calls
	secret string // secret / service_role key: server only, used to save and read results
}{
	strings.TrimRight(os.Getenv("SUPABASE_URL"), "/"),
	os.Getenv("SUPABASE_ANON_KEY"),
	os.Getenv("SUPABASE_SERVICE_ROLE_KEY"),
}

func authOn() bool { return supa.url != "" && supa.public != "" && supa.secret != "" }

var httpClient = &http.Client{Timeout: 10 * time.Second}

type user struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type session struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	User         user   `json:"user"`
}

// supaRequest calls a Supabase endpoint with the given key. Legacy keys are JWTs
// and also go in the Authorization header; new sb_publishable_/sb_secret_ keys go
// in apikey only. A user's access token, when given, is the Authorization instead.
func supaRequest(method, path, key, bearer string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, supa.url+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("apikey", key)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	} else if strings.HasPrefix(key, "eyJ") {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if strings.HasPrefix(path, "/rest/") && !strings.Contains(path, "/rpc/") {
		req.Header.Set("Prefer", "return=minimal")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return resp.StatusCode, errors.New(supaMessage(data))
	}
	if out != nil && len(data) > 0 {
		return resp.StatusCode, json.Unmarshal(data, out)
	}
	return resp.StatusCode, nil
}

// supaMessage pulls a readable message out of a Supabase error response.
func supaMessage(data []byte) string {
	var e struct {
		Msg, Message, ErrorDescription string
		Error                          any
	}
	json.Unmarshal(data, &e)
	for _, s := range []string{e.Msg, e.ErrorDescription, e.Message} {
		if s != "" {
			return s
		}
	}
	if s, ok := e.Error.(string); ok && s != "" {
		return s
	}
	return "Something went wrong. Please try again."
}

/* ---------- sessions ---------- */

// Access tokens are checked with Supabase once and then remembered until they expire.
var tokenCache = struct {
	sync.Mutex
	m map[string]cachedUser
}{m: map[string]cachedUser{}}

type cachedUser struct {
	user
	until time.Time
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func setSession(w http.ResponseWriter, r *http.Request, s session) {
	exp := s.ExpiresIn
	if exp <= 0 {
		exp = 3600
	}
	http.SetCookie(w, &http.Cookie{Name: "dai_at", Value: s.AccessToken, Path: "/", HttpOnly: true,
		Secure: secureRequest(r), SameSite: http.SameSiteLaxMode, MaxAge: exp})
	http.SetCookie(w, &http.Cookie{Name: "dai_rt", Value: s.RefreshToken, Path: "/", HttpOnly: true,
		Secure: secureRequest(r), SameSite: http.SameSiteLaxMode, MaxAge: 60 * 24 * 60 * 60})
	tokenCache.Lock()
	tokenCache.m[s.AccessToken] = cachedUser{s.User, time.Now().Add(time.Duration(exp) * time.Second)}
	tokenCache.Unlock()
}

func clearSession(w http.ResponseWriter) {
	for _, n := range []string{"dai_at", "dai_rt"} {
		http.SetCookie(w, &http.Cookie{Name: n, Value: "", Path: "/", MaxAge: -1})
	}
}

// currentUser returns the signed-in player, refreshing an expired session if needed.
// It makes network calls, so never call it while holding mu.
func currentUser(w http.ResponseWriter, r *http.Request) *user {
	if !authOn() {
		return nil
	}
	if c, err := r.Cookie("dai_at"); err == nil && c.Value != "" {
		tokenCache.Lock()
		cu, ok := tokenCache.m[c.Value]
		tokenCache.Unlock()
		if ok && time.Now().Before(cu.until) {
			return &cu.user
		}
		var u user
		if _, err := supaRequest("GET", "/auth/v1/user", supa.public, c.Value, nil, &u); err == nil && u.ID != "" {
			tokenCache.Lock()
			tokenCache.m[c.Value] = cachedUser{u, time.Now().Add(5 * time.Minute)}
			tokenCache.Unlock()
			return &u
		}
	}
	rt, err := r.Cookie("dai_rt")
	if err != nil || rt.Value == "" {
		return nil
	}
	var s session
	if _, err := supaRequest("POST", "/auth/v1/token?grant_type=refresh_token", supa.public, "",
		map[string]string{"refresh_token": rt.Value}, &s); err != nil || s.AccessToken == "" {
		clearSession(w)
		return nil
	}
	setSession(w, r, s)
	return &s.User
}

/* ---------- pages ---------- */

func showLogin(w http.ResponseWriter, email, errMsg, info string) {
	w.Header().Set("Content-Type", "text/html")
	loginPage.Execute(w, map[string]any{"Email": email, "Error": errMsg, "Info": info})
}

func loginPageHandler(w http.ResponseWriter, r *http.Request) {
	if !authOn() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if currentUser(w, r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	showLogin(w, "", "", "")
}

// authHandler handles both "Log in" and "Create account" from the login form.
func authHandler(w http.ResponseWriter, r *http.Request) {
	if !authOn() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")
	if _, err := mail.ParseAddress(email); err != nil {
		showLogin(w, email, "Enter a valid email address.", "")
		return
	}
	if len(password) < 6 {
		showLogin(w, email, "Passwords need at least 6 characters.", "")
		return
	}
	creds := map[string]string{"email": email, "password": password}

	if r.FormValue("action") == "signup" {
		// With "Confirm email" on, Supabase returns the user but no session until they confirm.
		var raw json.RawMessage
		if _, err := supaRequest("POST", "/auth/v1/signup", supa.public, "", creds, &raw); err != nil {
			showLogin(w, email, err.Error(), "")
			return
		}
		var s session
		json.Unmarshal(raw, &s)
		if s.AccessToken == "" {
			showLogin(w, email, "", "Account created. Check your email for a confirmation link, then log in.")
			return
		}
		setSession(w, r, s)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	var s session
	if _, err := supaRequest("POST", "/auth/v1/token?grant_type=password", supa.public, "", creds, &s); err != nil || s.AccessToken == "" {
		msg := "Wrong email or password."
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "confirm") {
			msg = "Please confirm your email first. Check your inbox for the link."
		}
		showLogin(w, email, msg, "")
		return
	}
	setSession(w, r, s)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("dai_at"); err == nil && authOn() {
		supaRequest("POST", "/auth/v1/logout", supa.public, c.Value, nil, nil)
		tokenCache.Lock()
		delete(tokenCache.m, c.Value)
		tokenCache.Unlock()
	}
	clearSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

/* ---------- saved results ---------- */

// soloResult is one finished solo game, as stored in the solo_games table.
type soloResult struct {
	UserID  string `json:"user_id"`
	Mode    string `json:"mode"`
	Won     bool   `json:"won"`
	Ms      *int64 `json:"ms"`
	Guesses int    `json:"guesses"`
}

func saveSoloResult(res soloResult) {
	if _, err := supaRequest("POST", "/rest/v1/solo_games", supa.secret, "", res, nil); err != nil {
		log.Printf("saving solo result for %s: %v", res.UserID, err)
	}
}

// modeStats is one mode's numbers from the solo_stats database function.
type modeStats struct {
	Played int      `json:"played"`
	Won    int      `json:"won"`
	Best   *float64 `json:"best"`
	Avg    *float64 `json:"avg"`
	Recent int      `json:"recent"`
}

func (s modeStats) BestText() string { return msText(s.Best) }
func (s modeStats) AvgText() string  { return msText(s.Avg) }

func msText(ms *float64) string {
	if ms == nil {
		return "–"
	}
	return clock(time.Duration(*ms) * time.Millisecond)
}

func loadSoloStats(userID string) (map[string]modeStats, error) {
	out := map[string]modeStats{}
	_, err := supaRequest("POST", "/rest/v1/rpc/solo_stats", supa.secret, "", map[string]string{"uid": userID}, &out)
	return out, err
}

func init() {
	if authOn() {
		fmt.Println("Supabase accounts on: " + supa.url)
	}
}
