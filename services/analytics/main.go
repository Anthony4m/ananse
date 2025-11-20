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
			"service": "analytics",
			"events":  []string{},
		})
	})

	log.Println("Analytics service listening on :5004")
	log.Fatal(http.ListenAndServe(":5004", nil))
}
