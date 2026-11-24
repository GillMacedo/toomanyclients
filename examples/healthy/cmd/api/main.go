// Command api serves the shop's HTTP API.
package main

import (
	"log"
	"net/http"

	"example.com/shop/healthy/internal/store"
)

func main() {
	db, err := store.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	http.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		var n int
		if err := db.QueryRowContext(r.Context(), "select count(*) from orders").Scan(&n); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	log.Fatal(http.ListenAndServe(":8080", nil))
}
