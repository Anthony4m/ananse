package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"ananse/pkg/database"
)

var (
	isHealthy   = true
	mu          sync.RWMutex
	serviceName string
	db          *database.PostgresDB

	// Fallback in-memory storage
	events    []Event
	eventsMu  sync.RWMutex
	maxEvents = 10000
)

// Event represents an analytics event
type Event struct {
	ID        string                 `json:"id,omitempty"`
	Type      string                 `json:"type"`
	UserID    string                 `json:"user_id,omitempty"`
	Data      map[string]interface{} `json:"data"`
	CreatedAt time.Time              `json:"created_at"`
}

func main() {
	serviceName = os.Getenv("SERVICE_NAME")
	if serviceName == "" {
		serviceName = "analytics"
	}

	// Initialize PostgreSQL connection
	ctx := context.Background()
	var err error
	db, err = database.WaitForDBFromEnv(ctx, "analytics", 30)
	if err != nil {
		log.Printf("[%s] Warning: PostgreSQL unavailable: %v", serviceName, err)
		// Continue without DB - we'll use in-memory fallback
	}

	log.Printf("[%s] Starting analytics service", serviceName)

	// Root endpoint with chaos simulation
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Chaos simulation: Latency
		sleep := r.URL.Query().Get("sleep")
		if sleep != "" {
			if sleepMs, err := strconv.Atoi(sleep); err == nil {
				log.Printf("[%s] Sleeping for %dms", serviceName, sleepMs)
				time.Sleep(time.Duration(sleepMs) * time.Millisecond)
			}
		}

		// Chaos simulation: Forced Status Code
		if codeStr := r.URL.Query().Get("code"); codeStr != "" {
			if code, err := strconv.Atoi(codeStr); err == nil {
				w.WriteHeader(code)
				if code >= 400 {
					json.NewEncoder(w).Encode(map[string]string{
						"error": fmt.Sprintf("Forced error: %d", code),
					})
					return
				}
			}
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"service":          serviceName,
			"events":           []string{},
			"received_headers": r.Header,
		})
	})

	// Health check (handle both /health and /analytics/health)
	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		if !isHealthy {
			http.Error(w, "Service Unhealthy", http.StatusInternalServerError)
			return
		}
		// Check DB health if available
		if db != nil {
			if err := db.HealthCheck(r.Context()); err != nil {
				log.Printf("[%s] DB health check failed: %v", serviceName, err)
			}
		}
		w.Write([]byte("Ok"))
	}
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/analytics/health", healthHandler)

	// Health toggle
	http.HandleFunc("/health/toggle", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		mu.Lock()
		isHealthy = !isHealthy
		newStatus := isHealthy
		mu.Unlock()

		msg := fmt.Sprintf("Health toggled to: %v", newStatus)
		log.Printf("[%s] %s", serviceName, msg)
		w.Write([]byte(msg))
	})

	// POST /analytics/event - Receive and store event
	http.HandleFunc("/analytics/event", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		// Chaos simulation
		if shouldFail(r, "analytics") {
			http.Error(w, "Analytics service unavailable", http.StatusServiceUnavailable)
			return
		}

		var eventData map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&eventData); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}

		eventType := getString(eventData, "type", "unknown")
		userID := getString(eventData, "user_id", "")

		eventID, err := storeEvent(r.Context(), eventType, userID, eventData)
		if err != nil {
			log.Printf("[%s] Failed to store event: %v", serviceName, err)
			http.Error(w, "Failed to store event", http.StatusInternalServerError)
			return
		}

		log.Printf("[%s] Event stored: %s (id: %s)", serviceName, eventType, eventID)

		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "stored",
			"event_id": eventID,
		})
	})

	// GET /analytics/events - Return recent events
	http.HandleFunc("/analytics/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		limitStr := r.URL.Query().Get("limit")
		limit := 100
		if limitStr != "" {
			if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
				limit = l
			}
		}

		eventType := r.URL.Query().Get("type")
		userID := r.URL.Query().Get("user_id")

		result, total := getEvents(r.Context(), limit, eventType, userID)

		json.NewEncoder(w).Encode(map[string]interface{}{
			"events": result,
			"count":  len(result),
			"total":  total,
		})
	})

	// GET /analytics/stats - Return aggregated stats
	http.HandleFunc("/analytics/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		stats := getStats(r.Context())

		json.NewEncoder(w).Encode(stats)
	})

	// POST /analytics/clear - Clear all events (testing endpoint)
	http.HandleFunc("/analytics/clear", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		count := clearEvents(r.Context())
		log.Printf("[%s] Cleared %d events", serviceName, count)
		w.Write([]byte(fmt.Sprintf("Cleared %d events", count)))
	})

	// Load testing endpoints
	http.HandleFunc("/chaos/cpu", func(w http.ResponseWriter, r *http.Request) {
		duration := r.URL.Query().Get("duration")
		if duration == "" {
			duration = "5"
		}
		secs, _ := strconv.Atoi(duration)
		log.Printf("[%s] CPU stress for %d seconds", serviceName, secs)
		start := time.Now()
		for time.Since(start) < time.Duration(secs)*time.Second {
			_ = time.Now().Unix()
		}
		w.Write([]byte(fmt.Sprintf("CPU stress completed for %d seconds", secs)))
	})

	http.HandleFunc("/chaos/memory", func(w http.ResponseWriter, r *http.Request) {
		size := r.URL.Query().Get("size")
		if size == "" {
			size = "100"
		}
		mb, _ := strconv.Atoi(size)
		log.Printf("[%s] Memory allocation: %d MB", serviceName, mb)
		data := make([]byte, mb*1024*1024)
		for i := range data {
			data[i] = byte(i % 256)
		}
		w.Write([]byte(fmt.Sprintf("Allocated %d MB", mb)))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "5004"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: nil,
	}

	go func() {
		log.Printf("[%s] Analytics service listening on :%s", serviceName, port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Printf("[%s] Shutting down server...", serviceName)

	// Close database connection
	if db != nil {
		db.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Printf("[%s] Server exiting", serviceName)
}

// storeEvent stores an event in the database
func storeEvent(ctx context.Context, eventType, userID string, data map[string]interface{}) (string, error) {
	if db != nil {
		jsonData, err := json.Marshal(data)
		if err != nil {
			return "", fmt.Errorf("failed to marshal event data: %w", err)
		}

		var eventID string
		err = db.Pool.QueryRow(ctx,
			`INSERT INTO events (type, user_id, data)
			 VALUES ($1, $2, $3)
			 RETURNING id`,
			eventType, nullIfEmpty(userID), jsonData).Scan(&eventID)

		if err != nil {
			return "", fmt.Errorf("failed to insert event: %w", err)
		}

		return eventID, nil
	}

	// Fallback: in-memory
	eventsMu.Lock()
	defer eventsMu.Unlock()

	eventID := fmt.Sprintf("evt-%d", time.Now().UnixNano())
	event := Event{
		ID:        eventID,
		Type:      eventType,
		UserID:    userID,
		Data:      data,
		CreatedAt: time.Now(),
	}

	events = append(events, event)
	if len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}

	return eventID, nil
}

// getEvents retrieves events with optional filtering
func getEvents(ctx context.Context, limit int, eventType, userID string) ([]Event, int) {
	if db != nil {
		// Build query
		query := "SELECT id, type, COALESCE(user_id, ''), data, created_at FROM events WHERE 1=1"
		args := []interface{}{}
		argNum := 1

		if eventType != "" {
			query += fmt.Sprintf(" AND type = $%d", argNum)
			args = append(args, eventType)
			argNum++
		}
		if userID != "" {
			query += fmt.Sprintf(" AND user_id = $%d", argNum)
			args = append(args, userID)
			argNum++
		}

		query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", argNum)
		args = append(args, limit)

		rows, err := db.Pool.Query(ctx, query, args...)
		if err != nil {
			log.Printf("[%s] Failed to query events: %v", serviceName, err)
			return []Event{}, 0
		}
		defer rows.Close()

		var result []Event
		for rows.Next() {
			var event Event
			var jsonData []byte
			if err := rows.Scan(&event.ID, &event.Type, &event.UserID, &jsonData, &event.CreatedAt); err != nil {
				continue
			}
			json.Unmarshal(jsonData, &event.Data)
			result = append(result, event)
		}

		// Get total count
		var total int
		countQuery := "SELECT COUNT(*) FROM events"
		db.Pool.QueryRow(ctx, countQuery).Scan(&total)

		return result, total
	}

	// Fallback: in-memory
	eventsMu.RLock()
	defer eventsMu.RUnlock()

	var filtered []Event
	for _, e := range events {
		if eventType != "" && e.Type != eventType {
			continue
		}
		if userID != "" && e.UserID != userID {
			continue
		}
		filtered = append(filtered, e)
	}

	start := 0
	if len(filtered) > limit {
		start = len(filtered) - limit
	}
	return filtered[start:], len(events)
}

// getStats returns aggregated statistics
func getStats(ctx context.Context) map[string]interface{} {
	if db != nil {
		var totalEvents int
		db.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM events").Scan(&totalEvents)

		rows, err := db.Pool.Query(ctx,
			"SELECT type, COUNT(*) FROM events GROUP BY type ORDER BY COUNT(*) DESC")
		if err != nil {
			log.Printf("[%s] Failed to get stats: %v", serviceName, err)
			return map[string]interface{}{
				"total_events": totalEvents,
				"by_type":      map[string]int{},
				"timestamp":    time.Now().Unix(),
			}
		}
		defer rows.Close()

		byType := make(map[string]int)
		for rows.Next() {
			var eventType string
			var count int
			if err := rows.Scan(&eventType, &count); err == nil {
				byType[eventType] = count
			}
		}

		return map[string]interface{}{
			"total_events": totalEvents,
			"by_type":      byType,
			"timestamp":    time.Now().Unix(),
			"storage":      "postgresql",
		}
	}

	// Fallback: in-memory
	eventsMu.RLock()
	defer eventsMu.RUnlock()

	byType := make(map[string]int)
	for _, event := range events {
		byType[event.Type]++
	}

	return map[string]interface{}{
		"total_events": len(events),
		"by_type":      byType,
		"timestamp":    time.Now().Unix(),
		"storage":      "in-memory",
	}
}

// clearEvents removes all events
func clearEvents(ctx context.Context) int {
	if db != nil {
		var count int
		db.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM events").Scan(&count)
		db.Pool.Exec(ctx, "TRUNCATE TABLE events")
		return count
	}

	// Fallback
	eventsMu.Lock()
	defer eventsMu.Unlock()
	count := len(events)
	events = []Event{}
	return count
}

// getString safely gets a string value from a map
func getString(m map[string]interface{}, key, defaultValue string) string {
	if val, ok := m[key].(string); ok {
		return val
	}
	return defaultValue
}

// nullIfEmpty returns nil if string is empty (for SQL NULL)
func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// shouldFail checks if we should simulate a failure based on query params
func shouldFail(r *http.Request, serviceName string) bool {
	failDownstream := r.URL.Query().Get("fail-downstream")
	if failDownstream == serviceName || failDownstream == "all" {
		return true
	}
	return false
}
