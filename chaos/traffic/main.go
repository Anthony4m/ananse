package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type TrafficGenerator struct {
	proxyURL   string
	authToken  string
	httpClient *http.Client

	// Statistics
	totalRequests   int64
	successRequests int64
	failedRequests  int64
	totalLatency    int64 // in microseconds

	// Per-endpoint stats
	endpointStats sync.Map // endpoint -> *EndpointStats

	// Latency buckets (for histogram)
	latencyBuckets [6]int64 // <10ms, <50ms, <100ms, <500ms, <1s, >1s

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type EndpointStats struct {
	Success int64
	Failed  int64
	Latency int64 // total microseconds
}

type Endpoint struct {
	Name     string
	Path     string
	Method   string
	Body     func() []byte // Function to generate request body
	Weight   int           // Higher weight = more likely to be called
	NeedAuth bool
}

type Config struct {
	ProxyURL    string
	RPS         int
	Duration    time.Duration
	Concurrency int
	Pattern     string // "uniform", "burst", "ramp", "cascade"
}

func NewTrafficGenerator(cfg Config) *TrafficGenerator {
	ctx, cancel := context.WithCancel(context.Background())

	return &TrafficGenerator{
		proxyURL: cfg.ProxyURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        200,
				MaxIdleConnsPerHost: 200,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		ctx:    ctx,
		cancel: cancel,
	}
}

func (t *TrafficGenerator) getEndpoints() []Endpoint {
	return []Endpoint{
		// Health checks (lightweight)
		{Name: "auth-health", Path: "/auth/health", Method: "GET", Weight: 10, NeedAuth: false},
		{Name: "users-health", Path: "/users/health", Method: "GET", Weight: 10, NeedAuth: false},
		{Name: "payments-health", Path: "/payments/health", Method: "GET", Weight: 10, NeedAuth: false},
		{Name: "analytics-health", Path: "/analytics/health", Method: "GET", Weight: 10, NeedAuth: false},

		// Auth endpoints
		{Name: "auth-login", Path: "/auth/login", Method: "POST", Weight: 15, NeedAuth: false,
			Body: func() []byte {
				b, _ := json.Marshal(map[string]string{
					"username": fmt.Sprintf("user%d", rand.Intn(100)),
					"password": "password123",
				})
				return b
			}},
		{Name: "auth-validate", Path: "/auth/validate", Method: "POST", Weight: 20, NeedAuth: true,
			Body: func() []byte { return []byte("{}") }},

		// Users endpoints (calls Auth)
		{Name: "users-profile", Path: "/users/profile", Method: "GET", Weight: 25, NeedAuth: true},
		{Name: "users-byid", Path: "/users/user-default", Method: "GET", Weight: 15, NeedAuth: true},
		{Name: "users-activity", Path: "/users/activity", Method: "POST", Weight: 10, NeedAuth: false,
			Body: func() []byte {
				b, _ := json.Marshal(map[string]string{
					"user_id":  fmt.Sprintf("user-%d", rand.Intn(100)),
					"activity": "page_view",
				})
				return b
			}},

		// Payments endpoints (calls Auth + Users + Analytics)
		{Name: "payments-balance", Path: "/payments/balance", Method: "GET", Weight: 20, NeedAuth: true},
		{Name: "payments-process", Path: "/payments/process", Method: "POST", Weight: 15, NeedAuth: true,
			Body: func() []byte {
				b, _ := json.Marshal(map[string]float64{
					"amount": float64(rand.Intn(100) + 1),
				})
				return b
			}},
		{Name: "payments-webhook", Path: "/payments/webhook", Method: "POST", Weight: 5, NeedAuth: false,
			Body: func() []byte {
				b, _ := json.Marshal(map[string]interface{}{
					"event":  "payment_received",
					"amount": rand.Intn(1000),
				})
				return b
			}},

		// Analytics endpoints
		{Name: "analytics-event", Path: "/analytics/event", Method: "POST", Weight: 15, NeedAuth: false,
			Body: func() []byte {
				b, _ := json.Marshal(map[string]interface{}{
					"type":    "page_view",
					"user_id": fmt.Sprintf("user-%d", rand.Intn(100)),
					"page":    "/home",
				})
				return b
			}},
		{Name: "analytics-events", Path: "/analytics/events?limit=10", Method: "GET", Weight: 10, NeedAuth: false},
		{Name: "analytics-stats", Path: "/analytics/stats", Method: "GET", Weight: 10, NeedAuth: false},
	}
}

func (t *TrafficGenerator) selectEndpoint(endpoints []Endpoint) Endpoint {
	totalWeight := 0
	for _, ep := range endpoints {
		totalWeight += ep.Weight
	}

	r := rand.Intn(totalWeight)
	cumulative := 0
	for _, ep := range endpoints {
		cumulative += ep.Weight
		if r < cumulative {
			return ep
		}
	}
	return endpoints[0]
}

func (t *TrafficGenerator) ensureToken() {
	if t.authToken != "" {
		return
	}

	// Login to get a token
	body, _ := json.Marshal(map[string]string{
		"username": "chaos-tester",
		"password": "password123",
	})

	resp, err := t.httpClient.Post(t.proxyURL+"/auth/login", "application/json", bytes.NewBuffer(body))
	if err != nil {
		t.authToken = "token-default" // Fallback token
		return
	}
	defer resp.Body.Close()

	var result struct {
		Token string `json:"token"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) == nil && result.Token != "" {
		t.authToken = result.Token
	} else {
		t.authToken = "token-default"
	}

	fmt.Printf("[TRAFFIC] Got auth token: %s...\n", t.authToken[:min(20, len(t.authToken))])
}

func (t *TrafficGenerator) sendRequest(ep Endpoint) (int, time.Duration, error) {
	start := time.Now()

	var body []byte
	if ep.Body != nil {
		body = ep.Body()
	}

	var req *http.Request
	var err error

	if body != nil {
		req, err = http.NewRequestWithContext(t.ctx, ep.Method, t.proxyURL+ep.Path, bytes.NewBuffer(body))
	} else {
		req, err = http.NewRequestWithContext(t.ctx, ep.Method, t.proxyURL+ep.Path, nil)
	}
	if err != nil {
		return 0, 0, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ananse-traffic-generator")
	req.Header.Set("X-Request-ID", fmt.Sprintf("chaos-%d", time.Now().UnixNano()))

	if ep.NeedAuth {
		t.ensureToken()
		req.Header.Set("Authorization", "Bearer "+t.authToken)
	}

	resp, err := t.httpClient.Do(req)
	duration := time.Since(start)

	if err != nil {
		return 0, duration, err
	}
	defer resp.Body.Close()

	return resp.StatusCode, duration, nil
}

func (t *TrafficGenerator) recordStats(ep Endpoint, status int, latency time.Duration, err error) {
	atomic.AddInt64(&t.totalRequests, 1)
	atomic.AddInt64(&t.totalLatency, latency.Microseconds())

	// Record latency bucket
	ms := latency.Milliseconds()
	switch {
	case ms < 10:
		atomic.AddInt64(&t.latencyBuckets[0], 1)
	case ms < 50:
		atomic.AddInt64(&t.latencyBuckets[1], 1)
	case ms < 100:
		atomic.AddInt64(&t.latencyBuckets[2], 1)
	case ms < 500:
		atomic.AddInt64(&t.latencyBuckets[3], 1)
	case ms < 1000:
		atomic.AddInt64(&t.latencyBuckets[4], 1)
	default:
		atomic.AddInt64(&t.latencyBuckets[5], 1)
	}

	// Per-endpoint stats
	statsI, _ := t.endpointStats.LoadOrStore(ep.Name, &EndpointStats{})
	stats := statsI.(*EndpointStats)

	if err != nil || status >= 400 {
		atomic.AddInt64(&t.failedRequests, 1)
		atomic.AddInt64(&stats.Failed, 1)
	} else {
		atomic.AddInt64(&t.successRequests, 1)
		atomic.AddInt64(&stats.Success, 1)
	}
	atomic.AddInt64(&stats.Latency, latency.Microseconds())
}

func (t *TrafficGenerator) worker(requestCh <-chan Endpoint) {
	t.wg.Add(1)
	defer t.wg.Done()

	for ep := range requestCh {
		status, latency, err := t.sendRequest(ep)
		t.recordStats(ep, status, latency, err)

		if err != nil {
			fmt.Printf("[FAIL] %s -> ERROR: %v\n", ep.Name, err)
		} else if status >= 400 {
			fmt.Printf("[FAIL] %s -> %d (%v)\n", ep.Name, status, latency)
		}
	}
}

func (t *TrafficGenerator) runUniform(rps int, duration time.Duration, concurrency int) {
	endpoints := t.getEndpoints()
	requestCh := make(chan Endpoint, concurrency*2)

	// Start workers
	for i := 0; i < concurrency; i++ {
		go t.worker(requestCh)
	}

	ticker := time.NewTicker(time.Second / time.Duration(rps))
	defer ticker.Stop()

	endTime := time.Now().Add(duration)

	for time.Now().Before(endTime) {
		select {
		case <-t.ctx.Done():
			close(requestCh)
			return
		case <-ticker.C:
			ep := t.selectEndpoint(endpoints)
			select {
			case requestCh <- ep:
			default:
				// Drop if workers overwhelmed
			}
		}
	}

	close(requestCh)
}

func (t *TrafficGenerator) runCascade(rps int, duration time.Duration, concurrency int) {
	// Cascade pattern: specifically test cascading calls
	// Focus on endpoints that trigger service-to-service communication
	cascadeEndpoints := []Endpoint{
		{Name: "payments-process", Path: "/payments/process", Method: "POST", Weight: 40, NeedAuth: true,
			Body: func() []byte {
				b, _ := json.Marshal(map[string]float64{"amount": float64(rand.Intn(50) + 1)})
				return b
			}},
		{Name: "users-profile", Path: "/users/profile", Method: "GET", Weight: 30, NeedAuth: true},
		{Name: "payments-balance", Path: "/payments/balance", Method: "GET", Weight: 30, NeedAuth: true},
	}

	requestCh := make(chan Endpoint, concurrency*2)

	for i := 0; i < concurrency; i++ {
		go t.worker(requestCh)
	}

	ticker := time.NewTicker(time.Second / time.Duration(rps))
	defer ticker.Stop()

	endTime := time.Now().Add(duration)

	for time.Now().Before(endTime) {
		select {
		case <-t.ctx.Done():
			close(requestCh)
			return
		case <-ticker.C:
			ep := t.selectEndpoint(cascadeEndpoints)
			select {
			case requestCh <- ep:
			default:
			}
		}
	}

	close(requestCh)
}

func (t *TrafficGenerator) runBurst(rps int, duration time.Duration, concurrency int) {
	endpoints := t.getEndpoints()
	requestCh := make(chan Endpoint, concurrency*10)

	for i := 0; i < concurrency; i++ {
		go t.worker(requestCh)
	}

	endTime := time.Now().Add(duration)

	for time.Now().Before(endTime) {
		select {
		case <-t.ctx.Done():
			close(requestCh)
			return
		default:
		}

		// Burst
		burstSize := rps * 2
		for i := 0; i < burstSize; i++ {
			ep := t.selectEndpoint(endpoints)
			select {
			case requestCh <- ep:
			default:
			}
		}

		// Pause
		pauseSeconds := rand.Intn(3) + 1
		select {
		case <-t.ctx.Done():
			close(requestCh)
			return
		case <-time.After(time.Duration(pauseSeconds) * time.Second):
		}
	}

	close(requestCh)
}

func (t *TrafficGenerator) PrintStats() {
	total := atomic.LoadInt64(&t.totalRequests)
	success := atomic.LoadInt64(&t.successRequests)
	failed := atomic.LoadInt64(&t.failedRequests)
	totalLatency := atomic.LoadInt64(&t.totalLatency)

	successRate := float64(0)
	avgLatency := float64(0)
	if total > 0 {
		successRate = float64(success) / float64(total) * 100
		avgLatency = float64(totalLatency) / float64(total) / 1000
	}

	fmt.Println("\n=============== Traffic Statistics ===============")
	fmt.Printf("Total Requests:    %d\n", total)
	fmt.Printf("Successful:        %d (%.2f%%)\n", success, successRate)
	fmt.Printf("Failed:            %d\n", failed)
	fmt.Printf("Avg Latency:       %.2f ms\n", avgLatency)

	fmt.Println("\nLatency Distribution:")
	fmt.Printf("  < 10ms:   %d\n", atomic.LoadInt64(&t.latencyBuckets[0]))
	fmt.Printf("  < 50ms:   %d\n", atomic.LoadInt64(&t.latencyBuckets[1]))
	fmt.Printf("  < 100ms:  %d\n", atomic.LoadInt64(&t.latencyBuckets[2]))
	fmt.Printf("  < 500ms:  %d\n", atomic.LoadInt64(&t.latencyBuckets[3]))
	fmt.Printf("  < 1s:     %d\n", atomic.LoadInt64(&t.latencyBuckets[4]))
	fmt.Printf("  > 1s:     %d\n", atomic.LoadInt64(&t.latencyBuckets[5]))

	fmt.Println("\nPer-Endpoint Stats:")
	t.endpointStats.Range(func(key, value interface{}) bool {
		name := key.(string)
		stats := value.(*EndpointStats)
		total := stats.Success + stats.Failed
		avgLat := float64(0)
		if total > 0 {
			avgLat = float64(stats.Latency) / float64(total) / 1000
		}
		fmt.Printf("  %-20s: %5d ok, %5d fail, avg %.1fms\n",
			name, stats.Success, stats.Failed, avgLat)
		return true
	})
	fmt.Println("==================================================")
}

func (t *TrafficGenerator) Run(cfg Config) {
	fmt.Println("========= Ananse Traffic Generator =========")
	fmt.Printf("Target:      %s\n", cfg.ProxyURL)
	fmt.Printf("RPS:         %d\n", cfg.RPS)
	fmt.Printf("Duration:    %v\n", cfg.Duration)
	fmt.Printf("Concurrency: %d\n", cfg.Concurrency)
	fmt.Printf("Pattern:     %s\n", cfg.Pattern)
	fmt.Println("============================================")

	// Get initial auth token
	t.ensureToken()

	// Stats printer
	statsTicker := time.NewTicker(15 * time.Second)
	go func() {
		for {
			select {
			case <-t.ctx.Done():
				return
			case <-statsTicker.C:
				t.PrintStats()
			}
		}
	}()

	switch cfg.Pattern {
	case "burst":
		t.runBurst(cfg.RPS, cfg.Duration, cfg.Concurrency)
	case "cascade":
		t.runCascade(cfg.RPS, cfg.Duration, cfg.Concurrency)
	default:
		t.runUniform(cfg.RPS, cfg.Duration, cfg.Concurrency)
	}

	t.wg.Wait()
	statsTicker.Stop()
	t.PrintStats()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func main() {
	cfg := Config{}

	flag.StringVar(&cfg.ProxyURL, "url", "http://localhost:8089", "Proxy URL")
	flag.IntVar(&cfg.RPS, "rps", 50, "Requests per second")
	flag.DurationVar(&cfg.Duration, "duration", 5*time.Minute, "Test duration")
	flag.IntVar(&cfg.Concurrency, "concurrency", 50, "Number of concurrent workers")
	flag.StringVar(&cfg.Pattern, "pattern", "uniform", "Traffic pattern: uniform, burst, cascade")
	flag.Parse()

	gen := NewTrafficGenerator(cfg)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nReceived interrupt signal...")
		gen.cancel()
	}()

	gen.Run(cfg)
}
