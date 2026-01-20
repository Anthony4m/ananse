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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	isHealthy   = true
	mu          sync.RWMutex
	proxyURL    string
	serviceName string
	httpClient  *http.Client
	db          *database.PostgresDB

	// Fallback in-memory storage
	balances  = make(map[string]float64)
	balanceMu sync.RWMutex
)

// Transaction represents a payment transaction
type Transaction struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Amount      float64   `json:"amount"`
	Type        string    `json:"type"`
	Status      string    `json:"status"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func main() {
	serviceName = os.Getenv("SERVICE_NAME")
	if serviceName == "" {
		serviceName = "payments"
	}

	proxyURL = os.Getenv("PROXY_URL")
	if proxyURL == "" {
		proxyURL = "http://localhost:8089"
	}

	// HTTP client with timeout
	httpClient = &http.Client{
		Timeout: 5 * time.Second,
	}

	// Initialize some default balances (fallback)
	balanceMu.Lock()
	balances["user-default"] = 1000.0
	balanceMu.Unlock()

	// Initialize PostgreSQL connection
	ctx := context.Background()
	var err error
	db, err = database.WaitForDBFromEnv(ctx, "payments", 30)
	if err != nil {
		log.Printf("[%s] Warning: PostgreSQL unavailable: %v", serviceName, err)
		// Continue without DB - we'll use in-memory fallback
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
			"balance":          1000,
			"received_headers": r.Header,
		})
	})

	// Health check (handle both /health and /payments/health)
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
	http.HandleFunc("/payments/health", healthHandler)

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

	// POST /payments/process - Calls AUTH to validate, calls USERS to get user, then processes payment
	http.HandleFunc("/payments/process", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		// Chaos simulation
		if shouldFail(r, "payments") {
			http.Error(w, "Payments service unavailable", http.StatusServiceUnavailable)
			return
		}

		requestID := getOrCreateRequestID(r)

		// Step 1: Validate token with Auth service
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Missing Authorization header", http.StatusUnauthorized)
			return
		}

		userID, err := validateTokenWithRetry(r.Context(), authHeader, requestID, 3)
		if err != nil {
			log.Printf("[%s] Auth validation failed: %v", serviceName, err)
			http.Error(w, fmt.Sprintf("Authentication failed: %v", err), http.StatusUnauthorized)
			return
		}

		// Step 2: Get user from Users service
		user, err := getUserWithRetry(r.Context(), userID, authHeader, requestID, 3)
		if err != nil {
			log.Printf("[%s] Failed to get user: %v", serviceName, err)
			http.Error(w, fmt.Sprintf("Users service unavailable: %v", err), http.StatusServiceUnavailable)
			return
		}

		// Step 3: Process payment
		var req struct {
			Amount      float64 `json:"amount"`
			Description string  `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}

		if req.Amount <= 0 {
			http.Error(w, "Invalid amount", http.StatusBadRequest)
			return
		}

		// Process payment with database transaction
		txnID, newBalance, err := processPayment(r.Context(), userID, req.Amount, req.Description)
		if err != nil {
			log.Printf("[%s] Payment processing failed: %v", serviceName, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Step 4: Log to Analytics (async, don't wait)
		go func() {
			event := map[string]interface{}{
				"type":           "payment_processed",
				"user_id":        userID,
				"amount":         req.Amount,
				"balance":        newBalance,
				"transaction_id": txnID,
				"timestamp":      time.Now().Unix(),
			}
			if err := callAnalytics(context.Background(), event, requestID); err != nil {
				log.Printf("[%s] Failed to log payment to analytics: %v", serviceName, err)
			}
		}()

		log.Printf("[%s] Payment processed: $%.2f for user %s (balance: $%.2f)", serviceName, req.Amount, userID, newBalance)

		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":         "processed",
			"user_id":        userID,
			"username":       user.Username,
			"amount":         req.Amount,
			"new_balance":    newBalance,
			"transaction_id": txnID,
		})
	})

	// GET /payments/balance - Calls AUTH, returns balance
	http.HandleFunc("/payments/balance", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		// Chaos simulation
		if shouldFail(r, "payments") {
			http.Error(w, "Payments service unavailable", http.StatusServiceUnavailable)
			return
		}

		requestID := getOrCreateRequestID(r)

		// Validate token
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Missing Authorization header", http.StatusUnauthorized)
			return
		}

		userID, err := validateTokenWithRetry(r.Context(), authHeader, requestID, 3)
		if err != nil {
			log.Printf("[%s] Auth validation failed: %v", serviceName, err)
			http.Error(w, fmt.Sprintf("Authentication failed: %v", err), http.StatusUnauthorized)
			return
		}

		balance := getBalance(r.Context(), userID)

		json.NewEncoder(w).Encode(map[string]interface{}{
			"user_id": userID,
			"balance": balance,
		})
	})

	// GET /payments/transactions - Get transaction history
	http.HandleFunc("/payments/transactions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		requestID := getOrCreateRequestID(r)

		// Validate token
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Missing Authorization header", http.StatusUnauthorized)
			return
		}

		userID, err := validateTokenWithRetry(r.Context(), authHeader, requestID, 3)
		if err != nil {
			http.Error(w, fmt.Sprintf("Authentication failed: %v", err), http.StatusUnauthorized)
			return
		}

		transactions := getTransactions(r.Context(), userID, 50)

		json.NewEncoder(w).Encode(map[string]interface{}{
			"user_id":      userID,
			"transactions": transactions,
			"count":        len(transactions),
		})
	})

	// POST /payments/webhook - Receives webhook, calls ANALYTICS to log
	http.HandleFunc("/payments/webhook", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		requestID := getOrCreateRequestID(r)

		var webhook map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&webhook); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}

		// Log to Analytics (async)
		go func() {
			event := map[string]interface{}{
				"type":      "payment_webhook",
				"data":      webhook,
				"timestamp": time.Now().Unix(),
			}
			if err := callAnalytics(context.Background(), event, requestID); err != nil {
				log.Printf("[%s] Failed to log webhook to analytics: %v", serviceName, err)
			} else {
				log.Printf("[%s] Webhook logged to analytics", serviceName)
			}
		}()

		json.NewEncoder(w).Encode(map[string]string{
			"status": "webhook_received",
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
		port = "5003"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: nil,
	}

	go func() {
		log.Printf("[%s] Payment service listening on :%s", serviceName, port)
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

// processPayment handles payment with database transaction
func processPayment(ctx context.Context, userID string, amount float64, description string) (string, float64, error) {
	txnID := uuid.New().String()

	if db != nil {
		// Use database transaction
		tx, err := db.Pool.Begin(ctx)
		if err != nil {
			return "", 0, fmt.Errorf("failed to start transaction: %w", err)
		}
		defer tx.Rollback(ctx)

		// Get or create balance
		var balance float64
		err = tx.QueryRow(ctx,
			"SELECT balance FROM balances WHERE user_id = $1 FOR UPDATE",
			userID).Scan(&balance)

		if err == pgx.ErrNoRows {
			// Create balance record with default
			balance = 1000.0
			_, err = tx.Exec(ctx,
				"INSERT INTO balances (user_id, balance) VALUES ($1, $2)",
				userID, balance)
			if err != nil {
				return "", 0, fmt.Errorf("failed to create balance: %w", err)
			}
		} else if err != nil {
			return "", 0, fmt.Errorf("failed to get balance: %w", err)
		}

		// Check sufficient funds
		if amount > balance {
			return "", balance, fmt.Errorf("insufficient balance: have $%.2f, need $%.2f", balance, amount)
		}

		// Deduct amount
		newBalance := balance - amount
		_, err = tx.Exec(ctx,
			"UPDATE balances SET balance = $1 WHERE user_id = $2",
			newBalance, userID)
		if err != nil {
			return "", 0, fmt.Errorf("failed to update balance: %w", err)
		}

		// Record transaction
		_, err = tx.Exec(ctx,
			`INSERT INTO transactions (id, user_id, amount, type, status, description)
			 VALUES ($1, $2, $3, 'debit', 'completed', $4)`,
			txnID, userID, amount, description)
		if err != nil {
			return "", 0, fmt.Errorf("failed to record transaction: %w", err)
		}

		// Commit
		if err := tx.Commit(ctx); err != nil {
			return "", 0, fmt.Errorf("failed to commit transaction: %w", err)
		}

		return txnID, newBalance, nil
	}

	// Fallback: in-memory
	balanceMu.Lock()
	defer balanceMu.Unlock()

	balance := balances[userID]
	if balance == 0 {
		balance = 1000.0
	}

	if amount > balance {
		return "", balance, fmt.Errorf("insufficient balance: have $%.2f, need $%.2f", balance, amount)
	}

	balances[userID] = balance - amount
	return txnID, balances[userID], nil
}

// getBalance retrieves user balance
func getBalance(ctx context.Context, userID string) float64 {
	if db != nil {
		var balance float64
		err := db.Pool.QueryRow(ctx,
			"SELECT balance FROM balances WHERE user_id = $1",
			userID).Scan(&balance)

		if err == nil {
			return balance
		}

		if err != pgx.ErrNoRows {
			log.Printf("[%s] Database error: %v", serviceName, err)
		}
	}

	// Fallback
	balanceMu.RLock()
	defer balanceMu.RUnlock()
	balance := balances[userID]
	if balance == 0 {
		balance = 1000.0
	}
	return balance
}

// getTransactions retrieves transaction history
func getTransactions(ctx context.Context, userID string, limit int) []Transaction {
	var transactions []Transaction

	if db != nil {
		rows, err := db.Pool.Query(ctx,
			`SELECT id, user_id, amount, type, status, COALESCE(description, ''), created_at
			 FROM transactions WHERE user_id = $1
			 ORDER BY created_at DESC LIMIT $2`,
			userID, limit)
		if err != nil {
			log.Printf("[%s] Failed to get transactions: %v", serviceName, err)
			return transactions
		}
		defer rows.Close()

		for rows.Next() {
			var txn Transaction
			if err := rows.Scan(&txn.ID, &txn.UserID, &txn.Amount, &txn.Type, &txn.Status, &txn.Description, &txn.CreatedAt); err != nil {
				continue
			}
			transactions = append(transactions, txn)
		}
	}

	return transactions
}

// validateTokenWithRetry calls Auth service with retry logic
func validateTokenWithRetry(ctx context.Context, authHeader, requestID string, maxRetries int) (string, error) {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			backoff := time.Duration(i) * 100 * time.Millisecond
			log.Printf("[%s] Retrying auth validation (attempt %d/%d) after %v", serviceName, i+1, maxRetries, backoff)
			time.Sleep(backoff)
		}

		userID, err := validateToken(ctx, authHeader, requestID)
		if err == nil {
			return userID, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("auth validation failed after %d retries: %v", maxRetries, lastErr)
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

// User represents a user
type User struct {
	ID       string  `json:"id"`
	Username string  `json:"username"`
	Email    string  `json:"email"`
	Balance  float64 `json:"balance"`
}

// getUserWithRetry calls Users service with retry logic
func getUserWithRetry(ctx context.Context, userID, authHeader, requestID string, maxRetries int) (*User, error) {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			backoff := time.Duration(i) * 100 * time.Millisecond
			log.Printf("[%s] Retrying get user (attempt %d/%d) after %v", serviceName, i+1, maxRetries, backoff)
			time.Sleep(backoff)
		}

		user, err := getUser(ctx, userID, authHeader, requestID)
		if err == nil {
			return user, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("get user failed after %d retries: %v", maxRetries, lastErr)
}

// getUser calls Users service to get user info
func getUser(ctx context.Context, userID, authHeader, requestID string) (*User, error) {
	url := fmt.Sprintf("%s/users/%s", proxyURL, userID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", authHeader)
	req.Header.Set("X-Request-ID", requestID)

	start := time.Now()
	resp, err := httpClient.Do(req)
	latency := time.Since(start)

	if err != nil {
		log.Printf("[%s] Users call failed (latency: %v): %v", serviceName, latency, err)
		return nil, fmt.Errorf("users service unavailable: %v", err)
	}
	defer resp.Body.Close()

	log.Printf("[%s] Users call completed (latency: %v, status: %d)", serviceName, latency, resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get user failed: %s", string(body))
	}

	var user User
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, fmt.Errorf("failed to decode user response: %v", err)
	}

	return &user, nil
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
