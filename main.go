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
	token string
	rows  []row
}

// match is one secret number being cracked by one player (solo) or raced by two (a room).
type match struct {
	size    int
	game    int
	secret  string
	players []*player
	winner  int // index into players, -1 until someone cracks it
	created time.Time
}

var mu sync.Mutex
var solo = newMatch(1)
var rooms = map[string]*match{}

func newMatch(size int) *match {
	m := &match{size: size, created: time.Now()}
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
	}
	m.winner = -1
}

func (m *match) started() bool { return len(m.players) == m.size }

func (m *match) guess(me int, g string) {
	dead, injured := 0, 0
	for i := 0; i < 4; i++ {
		if g[i] == m.secret[i] {
			dead++
		} else if strings.IndexByte(m.secret, g[i]) >= 0 {
			injured++
		}
	}
	p := m.players[me]
	p.rows = append(p.rows, row{len(p.rows) + 1, strings.Split(g, ""), dead, injured})
	if dead == 4 && m.winner < 0 {
		m.winner = me
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
	// In a race, a new round can only start once someone has won.
	if r.FormValue("new") != "" && (m.size == 1 || m.winner >= 0) {
		m.reset()
		http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
		return
	}
	if g := r.FormValue("guess"); isGuess(g) && m.started() && m.winner < 0 {
		m.guess(me, g)
		// Redirect so refreshing the page doesn't submit the guess again.
		http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
		return
	}

	scope := "solo"
	if code != "" {
		scope = code
	}
	v := map[string]any{
		"Room":    code,
		"Scope":   scope,
		"Game":    m.game,
		"Rows":    m.players[me].rows,
		"Waiting": !m.started(),
		"Over":    m.winner >= 0,
		"Won":     m.winner == me,
		"Lost":    m.winner >= 0 && m.winner != me,
	}
	if m.winner >= 0 {
		v["Secret"] = m.secret
		v["WinnerGuesses"] = len(m.players[m.winner].rows)
	}
	if m.size == 2 && m.started() {
		v["OppGuesses"] = len(m.players[1-me].rows)
	}
	w.Header().Set("Content-Type", "text/html")
	page.Execute(w, v)
}

func soloHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	play(w, r, solo, 0, "")
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
	rooms[code] = newMatch(2)
	http.Redirect(w, r, "/room/"+code, http.StatusSeeOther)
}

func joinHandler(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(r.FormValue("code")))
	http.Redirect(w, r, "/room/"+code, http.StatusSeeOther)
}

// seat finds which player this browser is in the room, joining it if there's a free spot.
func seat(w http.ResponseWriter, r *http.Request, m *match, code string) int {
	name := "dai_" + code
	if c, err := r.Cookie(name); err == nil {
		for i, p := range m.players {
			if p.token == c.Value {
				return i
			}
		}
	}
	if len(m.players) >= m.size {
		return -1
	}
	p := &player{token: token()}
	m.players = append(m.players, p)
	http.SetCookie(w, &http.Cookie{Name: name, Value: p.token, Path: "/", HttpOnly: true, MaxAge: 12 * 60 * 60})
	return len(m.players) - 1
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
		showMenu(w, "Room "+code+" already has two players.")
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
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{
		"game": m.game, "players": len(m.players), "winner": m.winner, "guesses": total,
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
	http.HandleFunc("/room/new", newRoomHandler)
	http.HandleFunc("/join", joinHandler)
	http.HandleFunc("/room/{code}", roomHandler)
	http.HandleFunc("/room/{code}/state", stateHandler)

	fmt.Println("Open http://localhost:8080")
	for _, ip := range lanAddrs() {
		fmt.Printf("Other devices on your network: http://%s:8080\n", ip)
	}
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Println(err)
	}
}
