<div align="center">
  <img src="ananse.png" alt="Ananse Mascot" width="200"/>
  <h1>Ananse</h1>
  <p><strong>A Resilient Service Mesh & Observability Plane in Go</strong></p>
  <p><em>Learning distributed systems through building production-grade infrastructure</em></p>

  ![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat&logo=go)
  ![License](https://img.shields.io/badge/license-MIT-green)
  ![Status](https://img.shields.io/badge/status-active%20development-blue)
</div>

---

## <img src="ananse.svg" width="30"/> What is Ananse?

**Ananse** is a production-ready service mesh data plane built in Go, named after the Akan folktale spider known for wisdom and cleverness. What began as a simple load balancer has evolved into a robust traffic management system featuring circuit breakers, intelligent retries, and comprehensive observability.

- **Current State:** Fully functional reverse proxy with advanced resilience patterns
- **Vision:** A complete service mesh with dynamic control plane, policy management, and distributed tracing

### Why Ananse?

This project is both a learning journey and a practical implementation of distributed systems patterns. Each feature is battle-tested through chaos engineering and high-concurrency stress tests, making it a valuable reference for understanding how production systems handle failure at scale.

## ✨ Key Features

### Traffic Management
- **Load Balancing Strategies**: Round-Robin and Least-Connections algorithms
- **Circuit Breaker Pattern**: Automatic failure detection with Open/HalfOpen/Closed state machine
- **Intelligent Retries**: Passive health checks with exponential backoff
- **Active Health Monitoring**: Continuous backend health checks with automatic recovery

### Resilience & Testing
- **Thread-Safe Backend Pool**: Atomic operations and RWMutex for safe concurrent access
- **Chaos Engineering**: Built-in chaos monkey for service disruption simulation
- **High-Velocity Load Testing**: Stress test scripts supporting 2000+ concurrent requests
- **Graceful Degradation**: Fail-fast behavior preventing cascading failures

### Observability
- **Prometheus Integration**: Scraped metrics for request counts, latencies, and circuit breaker states
- **Grafana Dashboards**: Pre-configured visualizations for system health
- **Alerting**: Threshold-based alerts for error rates and service availability

---

## 🚀 Getting Started

### Prerequisites

- **Go** (1.23+)
- **Docker** & **Docker Compose** (for monitoring stack)

### 1. Start the Infrastructure

First, spin up the monitoring stack (Prometheus & Grafana):

```bash
docker-compose up -d
```

### 2. Run the Services

Use the helper script to start the mesh and all backend microservices:

```bash
./start_services.sh
```

The mesh entry point will be running at `http://localhost:8089`.

### 3. Test the Proxy

Send requests through the mesh to any backend service:

```bash
# Route to auth service
curl http://localhost:8089/auth

# Route to users service
curl http://localhost:8089/users

# Route to payments service
curl http://localhost:8089/payments

# Route to analytics service
curl http://localhost:8089/analytics
```

The proxy will automatically load balance requests, handle failures with circuit breakers, and retry on transient errors.

### 4. Watch Resilience in Action

Open three terminals to observe the system under stress:

**Terminal 1** - Generate load:
```bash
./load_test.sh
```

**Terminal 2** - Introduce chaos:
```bash
./chaos_monkey.sh
```

**Terminal 3** - Watch metrics:
```bash
# Check circuit breaker states
curl http://localhost:8089/metrics | grep circuit_breaker_state

# Check success vs failure rates
curl http://localhost:8089/metrics | grep requests_total
```

Visit Grafana at [http://localhost:3000](http://localhost:3000) to see real-time dashboards showing:
- Request latency percentiles (p50, p95, p99)
- Circuit breaker state transitions
- Backend health status
- Error rate trends

## 🧪 Experiments

### Load Testing

Simulate high traffic with spikes and variable latency:

```bash
./load_test.sh
```

### Chaos Engineering (Local)

Unleash the Chaos Monkey to randomly kill and revive backend services:

```bash
./chaos_monkey.sh
```

### Kubernetes Chaos Engineering

For production-grade chaos testing in Kubernetes, use the comprehensive chaos testing toolkit in the `chaos/` directory.

#### Quick Start

```bash
# Port-forward the proxy (required for traffic generation)
kubectl port-forward -n ananse svc/proxy 8089:8089 &

# Run comprehensive traffic to all endpoints
cd chaos && ./chaos.sh traffic-full http://localhost:8089 50 5

# Start chaos loop (kills random pods every 30s for 5 minutes)
./chaos.sh chaos-loop 30 300
```

#### Available Commands

| Command | Description |
|---------|-------------|
| `kill-pod <service>` | Kill a random pod from a service |
| `scale <service> <replicas>` | Scale a service up or down |
| `network-delay <service> <latency>` | Add network latency (requires tc) |
| `restart-deploy <service>` | Rolling restart a deployment |
| `drain-node <node>` | Drain a node (careful!) |
| `chaos-loop <interval> <duration>` | Continuous random chaos |
| `traffic <url> <rps> <duration>` | Generate traffic to health endpoints |
| `traffic-full <url> <rps> <duration>` | Generate traffic to ALL endpoints |
| `traffic-cascade <url> <rps> <duration>` | Test service-to-service cascading |

#### Traffic Generator (Go)

For fine-grained control over traffic patterns:

```bash
cd chaos/traffic
go build -o traffic-gen .

# Uniform traffic at 100 RPS for 60 seconds
./traffic-gen -url http://localhost:8089 -rps 100 -duration 60s -pattern uniform

# Burst traffic (spikes to 5x RPS)
./traffic-gen -url http://localhost:8089 -rps 50 -duration 2m -pattern burst

# Cascade pattern (tests service-to-service communication)
./traffic-gen -url http://localhost:8089 -rps 30 -duration 2m -pattern cascade
```

#### Chaos Mesh (Production-Grade)

For advanced chaos scenarios, deploy [Chaos Mesh](https://chaos-mesh.org/):

```bash
# Install Chaos Mesh
kubectl apply -f https://mirrors.chaos-mesh.org/v2.6.0/crd.yaml
helm install chaos-mesh chaos-mesh/chaos-mesh -n chaos-mesh --create-namespace

# Apply chaos experiments
kubectl apply -f chaos/mesh/pod-kill.yaml      # Random pod failures
kubectl apply -f chaos/mesh/network-chaos.yaml # Network latency/loss
kubectl apply -f chaos/mesh/stress-chaos.yaml  # CPU/memory stress
kubectl apply -f chaos/mesh/workflow.yaml      # Combined chaos workflow
```

See `chaos/README.md` for detailed documentation on chaos testing strategies.

## 📊 Monitoring

- **Prometheus**: [http://localhost:9090](http://localhost:9090)
- **Grafana**: [http://localhost:3000](http://localhost:3000) (Login: `admin` / `admin`)

---

## 🗂️ Project Structure

```
ananse/
├── cmd/
│   ├── proxy/          # Main mesh data plane entry point
│   ├── auth/           # Auth microservice
│   ├── users/          # Users microservice
│   ├── payments/       # Payments microservice
│   └── analytics/      # Analytics microservice
├── pkg/
│   └── proxy/
│       ├── backend.go      # Backend pool & state management
│       ├── health.go       # Active health checking
│       ├── circuit.go      # Circuit breaker implementation
│       └── metrics.go      # Prometheus instrumentation
├── controlplane/       # Kubernetes control plane integration
│   ├── k8s-client.go       # K8s service discovery & informers
│   └── file-client.go      # File-based configuration
├── chaos/              # Kubernetes chaos engineering toolkit
│   ├── chaos.sh            # Main chaos testing script
│   ├── main.go             # Go-based chaos runner
│   ├── traffic/            # Traffic generator
│   │   └── main.go
│   └── mesh/               # Chaos Mesh manifests
│       ├── pod-kill.yaml
│       ├── network-chaos.yaml
│       ├── stress-chaos.yaml
│       └── workflow.yaml
├── k8s/                # Kubernetes deployment manifests
├── chaos_monkey.sh     # Local service disruption simulator
├── load_test.sh        # High-concurrency stress test
├── start_services.sh   # Start all services helper
└── docker-compose.yml  # Prometheus + Grafana stack
```

---

## 🤝 Contributing

Contributions are welcome! This project is both a learning exercise and a practical implementation reference. Areas where contributions would be valuable:

- **Control Plane**: Dynamic service discovery and configuration management
- **Distributed Tracing**: OpenTelemetry integration for request tracing
- **Policy Engine**: Rate limiting, access control, and traffic shaping
- **Documentation**: Architecture diagrams, runbooks, and tutorials
- **Testing**: Additional chaos scenarios and edge case coverage

### Getting Involved

1. Check the **Known Challenges** section for areas needing improvement
2. Review open issues or create one describing your proposed change
3. Fork the repository and create a feature branch
4. Ensure all tests pass and add new tests for your changes
5. Submit a pull request with a clear description

---

## 📄 License

MIT License