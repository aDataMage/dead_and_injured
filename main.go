package main

import (
	_ "embed"
	"fmt"
	"html/template"
	"math/rand"
	"net/http"
	"strings"
)

//go:embed page.html
var pageHTML string
var page = template.Must(template.New("page").Parse(pageHTML))

type row struct {
	N             int
	Digits        []string
	Dead, Injured int
}

var game int
var secret string
var rows []row
var won bool

func newGame() {
	game++
	secret = ""
	for _, d := range rand.Perm(10)[:4] {
		secret += fmt.Sprint(d)
	}
	rows = nil
	won = false
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

func handler(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("new") != "" {
		newGame()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	guess := r.FormValue("guess")
	if isGuess(guess) {
		if won {
			newGame()
		}
		dead, injured := 0, 0
		for i := 0; i < 4; i++ {
			if guess[i] == secret[i] {
				dead++
			} else if strings.IndexByte(secret, guess[i]) >= 0 {
				injured++
			}
		}
		rows = append(rows, row{len(rows) + 1, strings.Split(guess, ""), dead, injured})
		won = dead == 4
		// Redirect so refreshing the page doesn't submit the guess again.
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	page.Execute(w, map[string]any{"Rows": rows, "Won": won, "Secret": secret, "Game": game})
}

func main() {
	newGame()
	http.HandleFunc("/", handler)
	fmt.Println("Open http://localhost:8080")
	http.ListenAndServe(":8080", nil)
}
