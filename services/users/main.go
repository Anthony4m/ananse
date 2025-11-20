package main

import (
	"encoding/json"
	"log"
	"net/http"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"service": "users",
			"data":    []string{},
		})
	})

	log.Println("User service listening on :5002")
	log.Fatal(http.ListenAndServe(":5002", nil))
}
