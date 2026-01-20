package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"ananse/pkg/database"

	"github.com/jackc/pgx/v5"
)

var (
	isHealthy   = true
	mu          sync.RWMutex
	proxyURL    string
	serviceName string
	httpClient  *http.Client
	db          *database.PostgresDB
)

// User represents a user profile
type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

func main() {
	serviceName = os.Getenv("SERVICE_NAME")
	if serviceName == "" {
		serviceName = "users"
	}

	proxyURL = os.Getenv("PROXY_URL")
	if proxyURL == "" {
		proxyURL = "http://localhost:8089"
	}

	// HTTP client with timeout
	httpClient = &http.Client{
		Timeout: 5 * time.Second,
	}

	// Initialize PostgreSQL connection
	ctx := context.Background()
	var err error
	db, err = database.WaitForDBFromEnv(ctx, "users", 30)
	if err != nil {
		log.Printf("[%s] Warning: PostgreSQL unavailable: %v", serviceName, err)
		// Continue without DB - we'll generate users on the fly
	}

	log.Printf("[%s] Starting with PROXY_URL=%s", serviceName, proxyURL)

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
			"data":             []string{},
			"received_headers": r.Header,
		})
	})

	// Health check (handle both /health and /users/health)
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
		w.Write([]byte("OK"))
	}
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/users/health", healthHandler)

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

	// GET /users/profile - Calls AUTH to validate token, returns user profile
	http.HandleFunc("/users/profile", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		// Chaos simulation
		if shouldFail(r, "users") {
			http.Error(w, "Users service unavailable", http.StatusServiceUnavailable)
			return
		}

		// Get request ID for tracing
		requestID := getOrCreateRequestID(r)

		// Validate token with Auth service
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Missing Authorization header", http.StatusUnauthorized)
			return
		}

		userID, err := validateToken(r.Context(), authHeader, requestID)
		if err != nil {
			log.Printf("[%s] Auth validation failed: %v", serviceName, err)
			http.Error(w, fmt.Sprintf("Authentication failed: %v", err), http.StatusUnauthorized)
			return
		}

		// Get or create user from database
		user, err := getOrCreateUser(r.Context(), userID)
		if err != nil {
			log.Printf("[%s] Failed to get user: %v", serviceName, err)
			http.Error(w, "Failed to get user", http.StatusInternalServerError)
			return
		}

		log.Printf("[%s] Profile retrieved for user: %s", serviceName, user.ID)
		json.NewEncoder(w).Encode(user)
	})

	// GET /users/{id} - Get user by ID (calls AUTH first)
	http.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
		// Skip if it's the health endpoint
		if r.URL.Path == "/users/health" {
			return
		}

		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		// Chaos simulation
		if shouldFail(r, "users") {
			http.Error(w, "Users service unavailable", http.StatusServiceUnavailable)
			return
		}

		requestID := getOrCreateRequestID(r)

		// Validate token
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Missing Authorization header", http.StatusUnauthorized)
			return
		}

		_, err := validateToken(r.Context(), authHeader, requestID)
		if err != nil {
			log.Printf("[%s] Auth validation failed: %v", serviceName, err)
			http.Error(w, fmt.Sprintf("Authentication failed: %v", err), http.StatusUnauthorized)
			return
		}

		// Extract user ID from path (skip /users/ prefix)
		path := r.URL.Path
		if len(path) <= len("/users/") {
			http.Error(w, "Missing user ID", http.StatusBadRequest)
			return
		}
		userID := path[len("/users/"):]

		// Get user from database
		user, err := getOrCreateUser(r.Context(), userID)
		if err != nil {
			log.Printf("[%s] Failed to get user: %v", serviceName, err)
			http.Error(w, "Failed to get user", http.StatusInternalServerError)
			return
		}

		log.Printf("[%s] User retrieved: %s", serviceName, userID)
		json.NewEncoder(w).Encode(user)
	})

	// POST /users/activity - Log activity (calls ANALYTICS)
	http.HandleFunc("/users/activity", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		// Chaos simulation
		if shouldFail(r, "users") {
			http.Error(w, "Users service unavailable", http.StatusServiceUnavailable)
			return
		}

		requestID := getOrCreateRequestID(r)

		var req struct {
			UserID   string `json:"user_id"`
			Activity string `json:"activity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}

		// Call Analytics service asynchronously
		go func() {
			event := map[string]interface{}{
				"type":      "user_activity",
				"user_id":   req.UserID,
				"activity":  req.Activity,
				"timestamp": time.Now().Unix(),
			}
			if err := callAnalytics(context.Background(), event, requestID); err != nil {
				log.Printf("[%s] Failed to log activity to analytics: %v", serviceName, err)
			} else {
				log.Printf("[%s] Activity logged: %s for user %s", serviceName, req.Activity, req.UserID)
			}
		}()

		json.NewEncoder(w).Encode(map[string]string{
			"status": "activity_logged",
		})
	})

	// GET /users/list - List all users (admin endpoint)
	http.HandleFunc("/users/list", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		if db == nil {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"users": []User{},
				"error": "database not available",
			})
			return
		}

		rows, err := db.Pool.Query(r.Context(),
			"SELECT id, username, email, created_at FROM accounts ORDER BY created_at DESC LIMIT 100")
		if err != nil {
			log.Printf("[%s] Failed to list users: %v", serviceName, err)
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var users []User
		for rows.Next() {
			var user User
			if err := rows.Scan(&user.ID, &user.Username, &user.Email, &user.CreatedAt); err != nil {
				continue
			}
			users = append(users, user)
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"users": users,
			"count": len(users),
		})
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
		port = "5002"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: nil,
	}

	go func() {
		log.Printf("[%s] User service listening on :%s", serviceName, port)
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

// getOrCreateUser gets a user from DB or creates a synthetic one
func getOrCreateUser(ctx context.Context, userID string) (*User, error) {
	// Try to get from database first
	if db != nil {
		var user User
		err := db.Pool.QueryRow(ctx,
			"SELECT id, username, email, created_at FROM accounts WHERE username = $1 OR id::text = $1",
			userID).Scan(&user.ID, &user.Username, &user.Email, &user.CreatedAt)

		if err == nil {
			return &user, nil
		}

		if err != pgx.ErrNoRows {
			log.Printf("[%s] Database query error: %v", serviceName, err)
		}

		// User not found, try to create
		username := userID
		if len(username) > 5 && username[:5] == "user-" {
			username = username[5:]
		}

		var newID string
		err = db.Pool.QueryRow(ctx,
			`INSERT INTO accounts (username, email, password_hash)
			 VALUES ($1, $2, 'auto-generated')
			 ON CONFLICT (username) DO UPDATE SET updated_at = NOW()
			 RETURNING id, username, email, created_at`,
			username, fmt.Sprintf("%s@example.com", username)).
			Scan(&user.ID, &user.Username, &user.Email, &user.CreatedAt)

		if err != nil {
			log.Printf("[%s] Failed to create user: %v", serviceName, err)
			// Fall back to synthetic user
		} else {
			log.Printf("[%s] Created new user: %s (id: %s)", serviceName, username, newID)
			return &user, nil
		}
	}

	// Fallback: generate synthetic user
	return &User{
		ID:       userID,
		Username: userID,
		Email:    fmt.Sprintf("%s@example.com", userID),
	}, nil
}

// validateToken calls Auth service to validate a token
func validateToken(ctx context.Context, authHeader, requestID string) (string, error) {
	url := fmt.Sprintf("%s/auth/validate", proxyURL)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer([]byte("{}")))
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", authHeader)
	req.Header.Set("X-Request-ID", requestID)
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := httpClient.Do(req)
	latency := time.Since(start)

	if err != nil {
		log.Printf("[%s] Auth call failed (latency: %v): %v", serviceName, latency, err)
		return "", fmt.Errorf("auth service unavailable: %v", err)
	}
	defer resp.Body.Close()

	log.Printf("[%s] Auth call completed (latency: %v, status: %d)", serviceName, latency, resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("auth validation failed: %s", string(body))
	}

	var result struct {
		Valid  bool   `json:"valid"`
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode auth response: %v", err)
	}

	if !result.Valid {
		return "", fmt.Errorf("invalid token")
	}

	return result.UserID, nil
}

// callAnalytics calls Analytics service to log an event
func callAnalytics(ctx context.Context, event map[string]interface{}, requestID string) error {
	url := fmt.Sprintf("%s/analytics/event", proxyURL)

	body, err := json.Marshal(event)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}

	req.Header.Set("X-Request-ID", requestID)
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := httpClient.Do(req)
	latency := time.Since(start)

	if err != nil {
		log.Printf("[%s] Analytics call failed (latency: %v): %v", serviceName, latency, err)
		return err
	}
	defer resp.Body.Close()

	log.Printf("[%s] Analytics call completed (latency: %v, status: %d)", serviceName, latency, resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("analytics call failed: %s", string(body))
	}

	return nil
}

// getOrCreateRequestID gets X-Request-ID from header or generates a new one
func getOrCreateRequestID(r *http.Request) string {
	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = fmt.Sprintf("%s-%d", serviceName, time.Now().UnixNano())
	}
	return requestID
}

// shouldFail checks if we should simulate a failure based on query params
func shouldFail(r *http.Request, serviceName string) bool {
	failDownstream := r.URL.Query().Get("fail-downstream")
	if failDownstream == serviceName || failDownstream == "all" {
		return true
	}
	return false
}
