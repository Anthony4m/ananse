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
			"service": "payments",
			"balance": 1000,
		})
	})

	log.Println("Payment service listening on :5003")
	log.Fatal(http.ListenAndServe(":5003", nil))
}
