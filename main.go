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

var secret string
var rows []row
var won bool

func newGame() {
	secret = ""
	for _, d := range rand.Perm(10)[:4] {
		secret += fmt.Sprint(d)
	}
	rows = nil
	won = false
}

func handler(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("new") != "" {
		newGame()
	}
	guess := r.FormValue("guess")
	if len(guess) == 4 {
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
	}
	w.Header().Set("Content-Type", "text/html")
	page.Execute(w, map[string]any{"Rows": rows, "Won": won, "Secret": secret})
}

func main() {
	newGame()
	http.HandleFunc("/", handler)
	fmt.Println("Open http://localhost:8080")
	http.ListenAndServe(":8080", nil)
}
