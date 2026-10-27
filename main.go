package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	mrand "math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed page.html
var pageHTML string
var page = template.Must(template.New("page").Parse(pageHTML))

//go:embed menu.html
var menuHTML string
var menu = template.Must(template.New("menu").Parse(menuHTML))

type row struct {
	N             int
	Digits        []string
	Dead, Injured int
}

type player struct {
	token  string
	secret string // the number this player set for their opponent (duel only)
	rows   []row
	notes  notes // copy of the player's notes panel, used to show their opponent's progress bar
}

// notes mirrors the notes panel in the browser: a colour per digit and the digit locked into each position.
type notes struct {
	Marks [10]string `json:"marks"`
	Lock  [4]*int    `json:"lock"`
}

// cell is one digit of a player's own number in the duel top bar.
type cell struct {
	D     string
	Class string
}

// match is one secret number being cracked by one player (solo) or raced by two (a room).
// In a duel, each player instead sets their own number and cracks the other's.
type match struct {
	size    int
	duel    bool
	game    int
	secret  string
	players []*player
	winner  int // index into players, -1 until someone cracks it
	quit    int // index of the player who quit a room, -1 if nobody has
	created time.Time
	seen    time.Time // last request, so idle solo games can be cleared out

	// Solo timer. The clock starts on the first guess.
	limit    time.Duration // 0 means untimed
	start    time.Time
	finish   time.Time
	timedOut bool
}

var mu sync.Mutex
var solos = map[string]*match{} // per browser, keyed by cookie token and mode
var rooms = map[string]*match{}

const timedLimit = 120 * time.Second

func newMatch(size int) *match {
	m := &match{size: size, quit: -1, created: time.Now()}
	if size == 1 {
		m.players = []*player{{}}
	}
	m.reset()
	return m
}

func (m *match) reset() {
	m.game++
	m.secret = ""
	for _, d := range mrand.Perm(10)[:4] {
		m.secret += fmt.Sprint(d)
	}
	for _, p := range m.players {
		p.rows = nil
		p.secret = ""
		p.notes = notes{}
	}
	m.winner = -1
	m.start, m.finish, m.timedOut = time.Time{}, time.Time{}, false
}

// checkTime ends a timed game once its time is up.
func (m *match) checkTime() {
	if m.limit > 0 && !m.start.IsZero() && m.winner < 0 && !m.timedOut && time.Since(m.start) >= m.limit {
		m.timedOut = true
		m.finish = m.start.Add(m.limit)
	}
}

func (m *match) elapsed() time.Duration {
	switch {
	case m.start.IsZero():
		return 0
	case !m.finish.IsZero():
		return m.finish.Sub(m.start)
	}
	return time.Since(m.start)
}

func clock(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func (m *match) started() bool { return len(m.players) == m.size }

// ready reports whether guessing can begin: everyone has joined and, in a duel, set a number.
func (m *match) ready() bool {
	if !m.started() {
		return false
	}
	for _, p := range m.players {
		if m.duel && p.secret == "" {
			return false
		}
	}
	return true
}

// progress shows how much of player me's number the opponent has worked out in their notes:
// "green" if they've locked that digit into the right position, "yellow" if they've marked it as in the number.
func (m *match) progress(me int) []cell {
	secret := m.players[me].secret
	if !m.duel || secret == "" {
		return nil
	}
	var opp notes
	if m.started() {
		opp = m.players[1-me].notes
	}
	out := make([]cell, 4)
	for i := 0; i < 4; i++ {
		d := int(secret[i] - '0')
		out[i] = cell{D: secret[i : i+1]}
		lockedSomewhere := false
		for _, l := range opp.Lock {
			if l != nil && *l == d {
				lockedSomewhere = true
			}
		}
		switch {
		case opp.Lock[i] != nil && *opp.Lock[i] == d:
			out[i].Class = "green"
		case lockedSomewhere || opp.Marks[d] == "green" || opp.Marks[d] == "yellow":
			out[i].Class = "yellow"
		}
	}
	return out
}

// target is the number player me is trying to crack.
func (m *match) target(me int) string {
	if m.duel {
		return m.players[1-me].secret
	}
	return m.secret
}

func (m *match) guess(me int, g string) {
	secret := m.target(me)
	dead, injured := 0, 0
	for i := 0; i < 4; i++ {
		if g[i] == secret[i] {
			dead++
		} else if strings.IndexByte(secret, g[i]) >= 0 {
			injured++
		}
	}
	if m.start.IsZero() {
		m.start = time.Now()
	}
	p := m.players[me]
	p.rows = append(p.rows, row{len(p.rows) + 1, strings.Split(g, ""), dead, injured})
	if dead == 4 && m.winner < 0 {
		m.winner = me
		m.finish = time.Now()
	}
}

func isGuess(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isSecret checks a number a player sets: 4 digits, none repeated.
func isSecret(s string) bool {
	if !isGuess(s) {
		return false
	}
	for i := 0; i < 4; i++ {
		if strings.IndexByte(s[i+1:], s[i]) >= 0 {
			return false
		}
	}
	return true
}

func token() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// roomCode avoids letters that are easy to misread (I, O, L).
func roomCode() string {
	const letters = "ABCDEFGHJKMNPQRSTUVWXYZ"
	for {
		c := ""
		for i := 0; i < 4; i++ {
			c += string(letters[mrand.Intn(len(letters))])
		}
		if rooms[c] == nil {
			return c
		}
	}
}

func showMenu(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html")
	menu.Execute(w, map[string]any{"Error": msg})
}

func menuHandler(w http.ResponseWriter, r *http.Request) {
	showMenu(w, "")
}

// play handles guesses and restarts for a match, then renders it. Caller holds mu.
func play(w http.ResponseWriter, r *http.Request, m *match, me int, code string) {
	m.seen = time.Now()
	m.checkTime()
	// Quitting ends the room for both players; the opponent's page picks it up when it polls.
	if code != "" && r.Method == http.MethodPost && r.FormValue("quit") != "" {
		if m.quit < 0 {
			m.quit = me
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if m.quit >= 0 {
		v := map[string]any{"Room": code, "Scope": code, "Game": m.game, "Rows": m.players[me].rows,
			"Duel": m.duel, "Joined": m.started(), "Quit": true, "QuitByMe": m.quit == me}
		w.Header().Set("Content-Type", "text/html")
		page.Execute(w, v)
		return
	}
	// Coming from the menu: start a fresh solo game if the last one already ended,
	// otherwise pick up the unfinished one.
	if r.FormValue("from") == "menu" && m.size == 1 {
		if m.winner >= 0 || m.timedOut {
			m.reset()
		}
		http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
		return
	}
	// In a race, a new round can only start once someone has won.
	if r.FormValue("new") != "" && (m.size == 1 || m.winner >= 0) {
		m.reset()
		http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
		return
	}
	secretErr := ""
	if s := r.FormValue("secret"); s != "" && m.duel && m.players[me].secret == "" {
		if isSecret(s) {
			m.players[me].secret = s
			http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
			return
		}
		secretErr = "Your number needs exactly 4 digits, with no digit repeated."
	}
	if g := r.FormValue("guess"); isGuess(g) && m.ready() && m.winner < 0 && !m.timedOut {
		m.guess(me, g)
		// Redirect so refreshing the page doesn't submit the guess again.
		http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
		return
	}

	// The scope keeps browser-side notes from one game separate from another's.
	scope := code
	if code == "" {
		scope = fmt.Sprintf("solo%d-%d", m.limit/time.Second, m.created.Unix())
	}
	v := map[string]any{
		"Room":    code,
		"Scope":   scope,
		"Game":    m.game,
		"Rows":    m.players[me].rows,
		"Waiting": !m.ready(),
		"Joined":  m.started(),
		"Duel":    m.duel,
		"Over":    m.winner >= 0 || m.timedOut,
		"Won":     m.winner == me,
		"Lost":    m.winner >= 0 && m.winner != me,
		"Quit":    false,
	}
	if m.size == 1 {
		v["Solo"] = true
		v["LimitSec"] = int(m.limit / time.Second)
		v["ElapsedMs"] = m.elapsed().Milliseconds()
		v["Running"] = !m.start.IsZero() && m.winner < 0 && !m.timedOut
		v["TimeTaken"] = clock(m.elapsed())
		v["TimedOut"] = m.timedOut
		if m.timedOut {
			v["Secret"] = m.secret
		}
	}
	if m.duel {
		v["MySecret"] = m.players[me].secret
		v["Mine"] = m.progress(me)
		v["NeedSecret"] = m.players[me].secret == ""
		v["SecretError"] = secretErr
	}
	if m.winner >= 0 {
		v["Secret"] = m.target(me)
		v["WinnerGuesses"] = len(m.players[m.winner].rows)
	}
	if m.size == 2 && m.started() {
		v["OppGuesses"] = len(m.players[1-me].rows)
	}
	w.Header().Set("Content-Type", "text/html")
	page.Execute(w, v)
}

// soloHandler gives each browser its own solo game, one per mode.
func soloHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	tok := ""
	if c, err := r.Cookie("dai_solo"); err == nil {
		tok = c.Value
	} else {
		tok = token()
		http.SetCookie(w, &http.Cookie{Name: "dai_solo", Value: tok, Path: "/", HttpOnly: true, MaxAge: 30 * 24 * 60 * 60})
	}
	limit := time.Duration(0)
	if r.URL.Path == "/solo/timed" {
		limit = timedLimit
	}
	key := tok + r.URL.Path
	m := solos[key]
	if m == nil {
		for k, old := range solos {
			if time.Since(old.seen) > 12*time.Hour {
				delete(solos, k)
			}
		}
		m = newMatch(1)
		m.limit = limit
		solos[key] = m
	}
	play(w, r, m, 0, "")
}

func newRoomHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	for c, m := range rooms {
		if time.Since(m.created) > 12*time.Hour {
			delete(rooms, c)
		}
	}
	code := roomCode()
	m := newMatch(2)
	m.duel = r.FormValue("mode") == "duel"
	rooms[code] = m
	http.Redirect(w, r, "/room/"+code, http.StatusSeeOther)
}

func joinHandler(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(r.FormValue("code")))
	http.Redirect(w, r, "/room/"+code, http.StatusSeeOther)
}

// seat finds which player this browser is in the room, joining it if there's a free spot.
func seat(w http.ResponseWriter, r *http.Request, m *match, code string) int {
	if i := findSeat(r, m, code); i >= 0 {
		return i
	}
	if len(m.players) >= m.size || m.quit >= 0 {
		return -1
	}
	p := &player{token: token()}
	m.players = append(m.players, p)
	http.SetCookie(w, &http.Cookie{Name: "dai_" + code, Value: p.token, Path: "/", HttpOnly: true, MaxAge: 12 * 60 * 60})
	return len(m.players) - 1
}

// findSeat returns which player this browser already is in the room, or -1.
func findSeat(r *http.Request, m *match, code string) int {
	if c, err := r.Cookie("dai_" + code); err == nil {
		for i, p := range m.players {
			if p.token == c.Value {
				return i
			}
		}
	}
	return -1
}

// notesHandler stores a duel player's notes so their opponent's top bar can show their progress.
func notesHandler(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(r.PathValue("code"))
	var n notes
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&n); err != nil {
		http.Error(w, "bad notes", http.StatusBadRequest)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	m := rooms[code]
	if m == nil || !m.duel {
		http.NotFound(w, r)
		return
	}
	if me := findSeat(r, m, code); me >= 0 && m.game == gameParam(r) {
		m.players[me].notes = n
	}
	w.WriteHeader(http.StatusNoContent)
}

// gameParam reads which round a notes update belongs to, so stale notes from before a rematch are ignored.
func gameParam(r *http.Request) int {
	g, _ := strconv.Atoi(r.URL.Query().Get("game"))
	return g
}

func roomHandler(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(r.PathValue("code"))
	mu.Lock()
	defer mu.Unlock()
	m := rooms[code]
	if m == nil {
		showMenu(w, "There's no room with the code "+code+".")
		return
	}
	me := seat(w, r, m, code)
	if me < 0 {
		if m.quit >= 0 {
			showMenu(w, "The game in room "+code+" has ended.")
		} else {
			showMenu(w, "Room "+code+" already has two players.")
		}
		return
	}
	play(w, r, m, me, code)
}

// stateHandler lets a room page notice when the opponent joins, guesses, or wins.
func stateHandler(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(r.PathValue("code"))
	mu.Lock()
	defer mu.Unlock()
	m := rooms[code]
	if m == nil {
		http.NotFound(w, r)
		return
	}
	total := 0
	for _, p := range m.players {
		total += len(p.rows)
	}
	ready := 0
	if m.ready() {
		ready = 1
	}
	var mine []cell
	if me := findSeat(r, m, code); me >= 0 {
		mine = m.progress(me)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"mine": mine,
		"game": m.game, "players": len(m.players), "winner": m.winner, "guesses": total, "ready": ready,
		"quit": m.quit,
	})
}

// lanAddrs lists this computer's local network addresses so a second player can connect.
func lanAddrs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && !ip.IP.IsLoopback() && ip.IP.To4() != nil {
			out = append(out, ip.IP.String())
		}
	}
	return out
}

func main() {
	http.HandleFunc("/{$}", menuHandler)
	http.HandleFunc("/solo", soloHandler)
	http.HandleFunc("/solo/timed", soloHandler)
	http.HandleFunc("/room/new", newRoomHandler)
	http.HandleFunc("/join", joinHandler)
	http.HandleFunc("/room/{code}", roomHandler)
	http.HandleFunc("/room/{code}/state", stateHandler)
	http.HandleFunc("POST /room/{code}/notes", notesHandler)

	// Hosts like Render pick the port and pass it in PORT; locally we use 8080.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
		fmt.Println("Open http://localhost:8080")
		for _, ip := range lanAddrs() {
			fmt.Printf("Other devices on your network: http://%s:8080\n", ip)
		}
	} else {
		fmt.Println("Listening on port " + port)
	}
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		fmt.Println(err)
	}
}
