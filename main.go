package main

import (
	"fmt"
	"math/rand"
	"net/http"
	"strings"
)

var secret string
var history []string

func newGame() {
	secret = ""
	for _, d := range rand.Perm(10)[:4] {
		secret += fmt.Sprint(d)
	}
}

func handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	guess := r.FormValue("guess")
	if len(guess) == 4 {
		dead, injured := 0, 0
		for i := 0; i < 4; i++ {
			if guess[i] == secret[i] {
				dead++
			} else if strings.IndexByte(secret, guess[i]) >= 0 {
				injured++
			}
		}
		history = append(history, fmt.Sprintf("%s: %d dead, %d injured", guess, dead, injured))
		if dead == 4 {
			history = append(history, "You win! New game started.")
			newGame()
		}
	}
	fmt.Fprint(w, strings.Join(history, "<br>"))
	fmt.Fprint(w, `<form><input name="guess" autofocus><button>Guess</button></form>`)
}

func main() {
	newGame()
	http.HandleFunc("/", handler)
	fmt.Println("Open http://localhost:8080")
	http.ListenAndServe(":8080", nil)
}
