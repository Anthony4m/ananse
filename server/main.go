package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/echo", echoHandler)
	fmt.Println("Server starting on :4199")
	http.ListenAndServe(":4199", mux)
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sleep := r.URL.Query().Get("sleep")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	data := map[string]interface{}{
		"method":      r.Method,
		"path":        r.URL,
		"query":       r.Context(),
		"headers":     r.Header,
		"remote_addr": r.RemoteAddr,
		"sleep":       sleep,
	}

	// Convert to integer
	sleepMs, err := strconv.Atoi(sleep)
	if err != nil {
		http.Error(w, "Invalid sleep parameter", http.StatusBadRequest)
		return
	}

	// Use the value
	time.Sleep(time.Duration(sleepMs) * time.Millisecond)

	err = json.NewEncoder(w).Encode(data)
	if err != nil {
		return
	}
}
