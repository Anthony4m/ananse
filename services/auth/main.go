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
	redisDB     *database.RedisDB
	tokenTTL    = 24 * time.Hour
)

func main() {
	serviceName = os.Getenv("SERVICE_NAME")
	if serviceName == "" {
		serviceName = "auth"
	}

	// Initialize Redis connection
	ctx := context.Background()
	var err error
	redisDB, err = database.WaitForRedisFromEnv(ctx, 30)
	if err != nil {
		log.Printf("[%s] Warning: Redis unavailable, falling back to in-memory: %v", serviceName, err)
		// Continue without Redis - we'll use a fallback
	}

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
			"status":           "authenticated",
			"received_headers": r.Header,
		})
	})

	// Health check (handle both /health and /auth/health)
	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		if !isHealthy {
			http.Error(w, "Service Unhealthy", http.StatusInternalServerError)
			return
		}
		// Check Redis health if available
		if redisDB != nil {
			if err := redisDB.HealthCheck(r.Context()); err != nil {
				log.Printf("[%s] Redis health check failed: %v", serviceName, err)
			}
		}
		w.Write([]byte("Ok"))
	}
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/auth/health", healthHandler)

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

	// POST /auth/login - Mock login, return token
	http.HandleFunc("/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		// Chaos simulation
		if shouldFail(r, "auth") {
			http.Error(w, "Auth service unavailable", http.StatusServiceUnavailable)
			return
		}

		sleep := r.URL.Query().Get("sleep")
		if sleep != "" {
			if sleepMs, err := strconv.Atoi(sleep); err == nil {
				time.Sleep(time.Duration(sleepMs) * time.Millisecond)
			}
		}

		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}

		// Mock authentication - accept any username/password
		userID := fmt.Sprintf("user-%s", req.Username)
		if userID == "user-" {
			userID = "user-default"
		}

		// Generate token
		token := fmt.Sprintf("token-%d", time.Now().UnixNano())

		// Store token in Redis
		if redisDB != nil {
			if err := redisDB.SetToken(r.Context(), token, userID, tokenTTL); err != nil {
				log.Printf("[%s] Failed to store token in Redis: %v", serviceName, err)
				// Continue anyway - token validation will also check format
			}
		}

		log.Printf("[%s] Login successful for user: %s, token: %s...", serviceName, userID, token[:20])

		json.NewEncoder(w).Encode(map[string]interface{}{
			"token":   token,
			"user_id": userID,
			"expires": time.Now().Add(tokenTTL).Unix(),
		})
	})

	// POST /auth/validate - Validate token, return user_id
	http.HandleFunc("/auth/validate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		// Chaos simulation
		if shouldFail(r, "auth") {
			http.Error(w, "Auth service unavailable", http.StatusServiceUnavailable)
			return
		}

		sleep := r.URL.Query().Get("sleep")
		if sleep != "" {
			if sleepMs, err := strconv.Atoi(sleep); err == nil {
				time.Sleep(time.Duration(sleepMs) * time.Millisecond)
			}
		}

		// Get token from Authorization header or body
		var token string
		if authHeader := r.Header.Get("Authorization"); authHeader != "" {
			// Support "Bearer <token>" or just "<token>"
			if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
				token = authHeader[7:]
			} else {
				token = authHeader
			}
		} else {
			var req struct {
				Token string `json:"token"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "Invalid request", http.StatusBadRequest)
				return
			}
			token = req.Token
		}

		if token == "" {
			http.Error(w, "Missing token", http.StatusUnauthorized)
			return
		}

		var userID string

		// Try Redis first
		if redisDB != nil {
			tokenData, err := redisDB.GetToken(r.Context(), token)
			if err != nil {
				log.Printf("[%s] Redis error: %v", serviceName, err)
			} else if tokenData != nil {
				userID = tokenData.UserID
				log.Printf("[%s] Token validated from Redis for user: %s", serviceName, userID)
			}
		}

		// Fallback: accept tokens starting with "token-" (for testing/chaos scenarios)
		if userID == "" {
			if len(token) > 6 && token[:6] == "token-" {
				userID = "user-default"
				log.Printf("[%s] Token validated via fallback for user: %s", serviceName, userID)
			} else {
				http.Error(w, "Invalid token", http.StatusUnauthorized)
				return
			}
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"valid":   true,
			"user_id": userID,
		})
	})

	// POST /auth/logout - Invalidate token
	http.HandleFunc("/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		var token string
		if authHeader := r.Header.Get("Authorization"); authHeader != "" {
			if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
				token = authHeader[7:]
			} else {
				token = authHeader
			}
		}

		if token != "" && redisDB != nil {
			if err := redisDB.DeleteToken(r.Context(), token); err != nil {
				log.Printf("[%s] Failed to delete token: %v", serviceName, err)
			} else {
				log.Printf("[%s] Token invalidated", serviceName)
			}
		}

		json.NewEncoder(w).Encode(map[string]string{
			"status": "logged_out",
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
		port = "5001"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: nil,
	}

	go func() {
		log.Printf("[%s] Auth service listening on :%s", serviceName, port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Printf("[%s] Shutting down server...", serviceName)

	// Close Redis connection
	if redisDB != nil {
		redisDB.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Printf("[%s] Server exiting", serviceName)
}

// shouldFail checks if we should simulate a failure based on query params
func shouldFail(r *http.Request, serviceName string) bool {
	failDownstream := r.URL.Query().Get("fail-downstream")
	if failDownstream == serviceName || failDownstream == "all" {
		return true
	}
	return false
}
