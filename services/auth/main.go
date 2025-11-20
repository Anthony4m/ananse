package main

import (
	"encoding/json"
	"log"
	"net/http"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"service": "auth",
			"status":  "authenticated",
		})
	})

	log.Println("Auth service listening on :5001")
	log.Fatal(http.ListenAndServe(":5001", nil))
}
