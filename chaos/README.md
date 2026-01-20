# Ananse Chaos Testing

This directory contains chaos engineering tools to test the resilience of the Ananse service mesh under real-world failure conditions.

## Overview

Chaos testing helps validate that your mesh handles failures gracefully:
- Pod crashes and restarts
- Network latency and packet loss
- Resource exhaustion (CPU/memory)
- Rapid scaling events
- DNS failures
- Network partitions
- Database connection failures

## Architecture

The Ananse mesh includes a full database infrastructure for production-like testing:

```
                    ┌─────────────┐
                    │   Proxy     │
                    └──────┬──────┘
                           │
    ┌──────────┬───────────┼───────────┬──────────┐
    │          │           │           │          │
┌───▼───┐  ┌───▼───┐  ┌────▼────┐  ┌───▼─────┐   │
│ Auth  │  │ Users │  │Payments │  │Analytics│   │
└───┬───┘  └───┬───┘  └────┬────┘  └────┬────┘   │
    │          │           │            │        │
    ▼          │           │            │        │
┌───────┐      │           │            │        │
│ Redis │      │           │            │        │
└───────┘      │           │            │        │
               ▼           ▼            ▼        │
            ┌─────────────────────────────┐      │
            │        PostgreSQL           │      │
            │  ┌───────┐ ┌────────┐       │      │
            │  │users  │ │payments│       │      │
            │  │schema │ │schema  │       │      │
            │  └───────┘ └────────┘       │      │
            │  ┌─────────────────┐        │      │
            │  │analytics schema │        │      │
            │  └─────────────────┘        │      │
            └─────────────────────────────┘      │
```

### Database Components

| Component | Purpose | Storage |
|-----------|---------|---------|
| **PostgreSQL** | Primary data store | User accounts, balances, transactions, analytics events |
| **Redis** | Token cache | Auth tokens with TTL (24h expiry) |

### Service Data Flow

- **Auth**: Stores/validates tokens in Redis with automatic TTL expiration
- **Users**: CRUD operations on `users.accounts` table in PostgreSQL
- **Payments**: Balances and transactions with database-level atomicity
- **Analytics**: Events stored in PostgreSQL with JSONB for flexible querying

## Quick Start

### Full Environment Setup

The easiest way to deploy everything from scratch:

```bash
# From the repository root
./build_and_load.sh
```

This single command will:
1. Detect your Kind cluster
2. Create the `ananse` namespace
3. Deploy PostgreSQL and Redis
4. Wait for databases to be ready
5. Build all service images
6. Load images into the cluster
7. Apply all service manifests
8. Restart pods to pick up changes

### Deployment Options

```bash
# Full deployment (default)
./build_and_load.sh

# Skip infrastructure (databases already running)
./build_and_load.sh --skip-infra

# Skip build (just apply manifests)
./build_and_load.sh --skip-build

# Deploy specific services only
./build_and_load.sh auth users

# Rebuild single service
./build_and_load.sh --skip-infra payments
```

### Option 1: Bash Script (Immediate Use)

The simplest way to start chaos testing:

```bash
# Make sure you're connected to your cluster
kubectl config current-context

# Check service status
./chaos.sh status

# Generate traffic in background
./chaos.sh traffic-loop 50 300 &

# Run chaos
./chaos.sh kill-pod auth
./chaos.sh scale-bounce users 2 15
./chaos.sh chaos-loop 30 120
```

### Option 2: Go-Based Chaos Tool

More sophisticated testing with concurrent traffic and chaos:

```bash
cd chaos
go build -o chaos-runner .

# Run with defaults (5 min, 50 rps, chaos every 30s)
./chaos-runner

# Custom configuration
./chaos-runner \
  --duration=10m \
  --rps=100 \
  --interval=15s \
  --pod-kill=true \
  --scaling=true \
  --rolling=false
```

### Option 3: Chaos Mesh (Production-Grade)

For comprehensive chaos engineering with a UI:

```bash
# Install Chaos Mesh
./chaos.sh install-chaos-mesh

# Access dashboard
kubectl port-forward -n chaos-mesh svc/chaos-dashboard 2333:2333
# Open http://localhost:2333

# Apply chaos experiments
kubectl apply -f mesh/pod-kill.yaml
kubectl apply -f mesh/network-chaos.yaml
kubectl apply -f mesh/workflow.yaml
```

## Tools Reference

### chaos.sh - Bash Script

| Command | Description |
|---------|-------------|
| `kill-pod [svc]` | Kill a random pod |
| `kill-service [svc]` | Kill all pods of a service |
| `scale-bounce [svc] [rep] [sec]` | Scale to 0 then back |
| `scale-up [svc] [rep]` | Scale up a service |
| `scale-down [svc] [rep]` | Scale down a service |
| `rolling-restart [svc]` | Trigger rolling restart |
| `partial-outage [svc]` | Kill half the pods |
| `dns-blackhole [svc] [sec]` | Break DNS temporarily |
| `resource-pressure [svc] [sec]` | Apply memory pressure |
| `scale-chaos [svc] [sec]` | Random scaling |
| `chaos-loop [interval] [dur]` | Continuous pod killing |
| `full-chaos [duration]` | Combined chaos types |
| `traffic [n] [c] [ep]` | Generate traffic |
| `traffic-loop [rps] [dur]` | Continuous traffic |
| `status` | Show service status |
| `reset` | Reset all services |

### Go Chaos Runner

```
Usage:
  chaos-runner [flags]

Flags:
  --namespace     Kubernetes namespace (default: ananse)
  --proxy-url     Proxy URL (default: http://localhost:8089)
  --duration      Test duration (default: 5m)
  --rps           Traffic requests per second (default: 50)
  --interval      Chaos action interval (default: 30s)
  --pod-kill      Enable pod killing (default: true)
  --scaling       Enable random scaling (default: true)
  --rolling       Enable rolling restarts (default: false)
```

### Traffic Generator

```bash
cd chaos/traffic
go build -o traffic-gen .

./traffic-gen \
  --url=http://localhost:8089 \
  --rps=100 \
  --duration=10m \
  --concurrency=50 \
  --pattern=uniform  # uniform, burst, ramp
```

## Chaos Mesh Experiments

### Pod Chaos (`mesh/pod-kill.yaml`)

- **pod-kill-random**: Kill random mesh pod every 2 minutes
- **pod-kill-auth**: Kill auth service pod every 5 minutes
- **pod-failure**: Mark pod as failed for 60 seconds

### Network Chaos (`mesh/network-chaos.yaml`)

- **network-delay-auth**: 200ms latency + 50ms jitter
- **network-delay-high**: 500ms latency (simulates cross-region)
- **network-loss**: 10% packet loss
- **network-partition**: Isolate service from proxy
- **network-bandwidth**: Limit to 1mbps
- **dns-error**: DNS resolution failures

### Stress Chaos (`mesh/stress-chaos.yaml`)

- **cpu-stress**: 80% CPU load for 2 minutes
- **memory-stress**: 100Mi memory consumption
- **combined-stress**: 50% CPU + 50Mi memory

### Workflows (`mesh/workflow.yaml`)

- **mesh-resilience-test**: Full orchestrated test sequence
- **quick-chaos-test**: Quick 2-minute test
- **scheduled-chaos**: Runs every 4 hours

## Testing Scenarios

### Scenario 0: Database Chaos

Tests resilience to database failures. Services have graceful fallback to in-memory storage.

```bash
# Terminal 1: Generate traffic
./chaos.sh traffic-loop 100 300

# Terminal 2: Kill PostgreSQL
kubectl delete pod -l app=postgres -n ananse

# Terminal 3: Kill Redis
kubectl delete pod -l app=redis -n ananse
```

**What to watch:**
- Services continue operating with in-memory fallback
- Logs show "failed to connect" warnings, not crashes
- Data created during outage won't persist after recovery
- Services reconnect automatically when DB comes back

**Testing token persistence:**
```bash
# Get a token
curl -X POST http://localhost:8089/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"test","password":"test"}'

# Kill Redis, token should still work (fallback to memory)
kubectl delete pod -l app=redis -n ananse

# Validate token still works
curl http://localhost:8089/auth/validate \
  -H "Authorization: Bearer <token>"
```

**Testing payment atomicity:**
```bash
# Payments use database transactions - kills during processing
# should not leave partial state

# Start continuous payments
for i in {1..100}; do
  curl -X POST http://localhost:8089/payments/process \
    -H "Content-Type: application/json" \
    -d '{"user_id":"test","amount":10}' &
done

# Kill postgres mid-transaction
kubectl delete pod -l app=postgres -n ananse
```

### Scenario 1: Pod Recovery

Tests how the mesh handles pod failures.

```bash
# Terminal 1: Generate traffic
./chaos.sh traffic-loop 100 300

# Terminal 2: Kill pods
./chaos.sh kill-pod auth
sleep 30
./chaos.sh kill-pod users
```

**What to watch:**
- Circuit breaker opens after consecutive failures
- Load balancer routes to healthy backends
- Endpoint discovery updates within 500ms
- Metrics show spike then recovery

### Scenario 2: Network Degradation

Tests retry logic and timeouts.

```bash
# Apply network delay
kubectl apply -f mesh/network-chaos.yaml

# Generate traffic and watch latency
./chaos/traffic/traffic-gen --rps=50 --duration=5m
```

**What to watch:**
- Latency histogram shifts right
- No increase in error rate (retries work)
- Circuit breaker may open if latency > threshold

### Scenario 3: Scaling Stress

Tests endpoint discovery under rapid changes.

```bash
# Terminal 1: Traffic
./chaos.sh traffic-loop 200 600

# Terminal 2: Rapid scaling
./chaos.sh scale-chaos auth 300
```

**What to watch:**
- Control plane debounces updates (500ms)
- No requests to stale endpoints
- Load distribution adjusts to new pod count

### Scenario 4: Full Resilience Test

Comprehensive test combining multiple failures.

```bash
# Using Chaos Mesh workflow
kubectl apply -f mesh/workflow.yaml

# OR using bash script
./chaos.sh traffic-loop 100 600 &
./chaos.sh full-chaos 300
```

## Metrics to Monitor

During chaos testing, watch these Prometheus metrics:

```promql
# Request success rate
rate(ananse_http_requests_total{status=~"2.."}[1m])
/ rate(ananse_http_requests_total[1m])

# Circuit breaker state (0=closed, 1=half-open, 2=open)
ananse_circuit_breaker_state

# Backend health
ananse_backend_health_status

# Retry rate
rate(ananse_retry_attempts_total[1m])

# Request latency p99
histogram_quantile(0.99, rate(ananse_http_request_duration_seconds_bucket[1m]))
```

## Production Hardening Checklist

Based on chaos testing results, verify:

### Mesh Resilience
- [ ] Circuit breaker opens after 5 consecutive failures
- [ ] Circuit breaker recovers within 30-60 seconds
- [ ] Retries succeed on transient failures (3 attempts max)
- [ ] Health checks detect unhealthy backends within 10 seconds
- [ ] Endpoint discovery updates within 500ms of pod changes
- [ ] No requests sent to terminating pods (graceful drain)
- [ ] Load balancer redistributes on backend failure
- [ ] 99th percentile latency stays under SLO during chaos
- [ ] Success rate stays above SLO during partial outages
- [ ] Graceful degradation when entire service is down

### Database Resilience
- [ ] Services start without databases (graceful fallback)
- [ ] Services reconnect when databases become available
- [ ] Token validation works during Redis outage (in-memory fallback)
- [ ] Payment transactions are atomic (no partial state on kill)
- [ ] Connection pools recover from database restarts
- [ ] Data persists across service pod restarts
- [ ] PostgreSQL PVC retains data across postgres pod restarts

## Recommendations

### For Development/Staging

1. Run `chaos.sh` manually to understand failure modes
2. Use Go chaos runner for automated testing in CI
3. Start with single chaos types, then combine

### For Production

1. Install Chaos Mesh for controlled experiments
2. Start with scheduled low-impact chaos (pod-kill every 4h)
3. Use workflows for comprehensive testing during maintenance windows
4. Set up alerting on chaos experiments
5. Gradually increase chaos frequency as confidence grows

### Best Practices

1. **Always have traffic**: Chaos without traffic doesn't validate recovery
2. **Monitor metrics**: Watch dashboards during experiments
3. **Start small**: Single pod kill before full chaos
4. **Have rollback**: Know how to stop chaos quickly (`./chaos.sh reset`)
5. **Document findings**: Record what breaks and how you fixed it

## Useful Commands

### Database Access

```bash
# Connect to PostgreSQL
kubectl exec -it deploy/postgres -n ananse -- psql -U ananse -d ananse

# View users table
kubectl exec -it deploy/postgres -n ananse -- psql -U ananse -d ananse -c "SELECT * FROM users.accounts;"

# View payment balances
kubectl exec -it deploy/postgres -n ananse -- psql -U ananse -d ananse -c "SELECT * FROM payments.balances;"

# View recent transactions
kubectl exec -it deploy/postgres -n ananse -- psql -U ananse -d ananse -c "SELECT * FROM payments.transactions ORDER BY created_at DESC LIMIT 10;"

# View analytics events
kubectl exec -it deploy/postgres -n ananse -- psql -U ananse -d ananse -c "SELECT * FROM analytics.events ORDER BY created_at DESC LIMIT 10;"

# Connect to Redis
kubectl exec -it deploy/redis -n ananse -- redis-cli -a ananse-redis-pw

# List all tokens in Redis
kubectl exec -it deploy/redis -n ananse -- redis-cli -a ananse-redis-pw KEYS "tokens:*"

# Get a specific token
kubectl exec -it deploy/redis -n ananse -- redis-cli -a ananse-redis-pw GET "tokens:<token-id>"
```

### Service Logs

```bash
# View all service logs
kubectl logs -f deploy/auth -n ananse
kubectl logs -f deploy/users -n ananse
kubectl logs -f deploy/payments -n ananse
kubectl logs -f deploy/analytics -n ananse

# View database logs
kubectl logs -f deploy/postgres -n ananse
kubectl logs -f deploy/redis -n ananse

# Follow all mesh pods
kubectl logs -f -l app.kubernetes.io/part-of=ananse-mesh -n ananse
```

### Troubleshooting

```bash
# Check database connectivity
kubectl exec -it deploy/auth -n ananse -- nc -zv redis 6379
kubectl exec -it deploy/users -n ananse -- nc -zv postgres 5432

# Check service health
for svc in auth users payments analytics; do
  echo "$svc: $(kubectl exec -it deploy/$svc -n ananse -- wget -qO- localhost:500{1,2,3,4}/health 2>/dev/null || echo 'unhealthy')"
done

# Reset databases (WARNING: deletes all data)
kubectl delete pvc postgres-data -n ananse
kubectl rollout restart deploy/postgres -n ananse
kubectl rollout restart deploy/redis -n ananse
```
