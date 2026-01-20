#!/bin/bash
# Ananse Service Mesh Chaos Testing Script
# Usage: ./chaos.sh <command> [options]

set -e

NAMESPACE="${NAMESPACE:-ananse}"
SERVICES=("auth" "users" "payments" "analytics")
PROXY_URL="${PROXY_URL:-http://localhost:8089}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Check if kubectl is available and cluster is accessible
check_prereqs() {
    if ! command -v kubectl &> /dev/null; then
        log_error "kubectl not found. Please install kubectl."
        exit 1
    fi

    if ! kubectl get ns "$NAMESPACE" &> /dev/null; then
        log_error "Namespace '$NAMESPACE' not found or cluster not accessible."
        exit 1
    fi
}

# Get random service from the list
get_random_service() {
    echo "${SERVICES[$RANDOM % ${#SERVICES[@]}]}"
}

# Get random pod from a deployment (macOS compatible)
get_random_pod() {
    local service=$1
    local pods=$(kubectl get pods -n "$NAMESPACE" -l "app=$service" --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}')
    local pod_array=($pods)
    local count=${#pod_array[@]}

    if [ $count -eq 0 ]; then
        return
    fi

    echo "${pod_array[$((RANDOM % count))]}"
}

# ==================== CHAOS COMMANDS ====================

# Kill a random pod
cmd_kill_pod() {
    local service=${1:-$(get_random_service)}
    local pod=$(get_random_pod "$service")

    if [ -z "$pod" ]; then
        log_warn "No running pods found for service: $service"
        return 1
    fi

    log_info "Killing pod: $pod (service: $service)"
    kubectl delete pod "$pod" -n "$NAMESPACE" --grace-period=0 --force
    log_success "Pod $pod killed. Kubernetes will recreate it."
}

# Kill all pods of a service
cmd_kill_service() {
    local service=${1:-$(get_random_service)}
    log_info "Killing all pods for service: $service"
    kubectl delete pods -n "$NAMESPACE" -l "app=$service" --grace-period=0 --force
    log_success "All pods for $service killed."
}

# Scale a service down to 0 then back up
cmd_scale_bounce() {
    local service=${1:-$(get_random_service)}
    local original_replicas=${2:-2}
    local downtime=${3:-10}

    log_info "Scaling $service to 0 replicas for ${downtime}s"
    kubectl scale deployment "$service" -n "$NAMESPACE" --replicas=0

    sleep "$downtime"

    log_info "Scaling $service back to $original_replicas replicas"
    kubectl scale deployment "$service" -n "$NAMESPACE" --replicas="$original_replicas"
    log_success "Scale bounce complete for $service"
}

# Scale up rapidly to test endpoint discovery
cmd_scale_up() {
    local service=${1:-$(get_random_service)}
    local replicas=${2:-5}

    log_info "Scaling $service to $replicas replicas"
    kubectl scale deployment "$service" -n "$NAMESPACE" --replicas="$replicas"
    log_success "Scaled $service to $replicas replicas"
}

# Scale down
cmd_scale_down() {
    local service=${1:-$(get_random_service)}
    local replicas=${2:-1}

    log_info "Scaling $service to $replicas replica(s)"
    kubectl scale deployment "$service" -n "$NAMESPACE" --replicas="$replicas"
    log_success "Scaled $service to $replicas replica(s)"
}

# Rolling restart
cmd_rolling_restart() {
    local service=${1:-$(get_random_service)}
    log_info "Rolling restart of $service"
    kubectl rollout restart deployment "$service" -n "$NAMESPACE"
    log_success "Rolling restart initiated for $service"
}

# Network latency injection using tc (requires privileged pods or Chaos Mesh)
cmd_latency_simple() {
    local service=${1:-$(get_random_service)}
    local delay_ms=${2:-500}
    local pod=$(get_random_pod "$service")

    if [ -z "$pod" ]; then
        log_warn "No running pods found for service: $service"
        return 1
    fi

    log_warn "Network latency injection requires either:"
    log_warn "  1. Chaos Mesh installed (recommended)"
    log_warn "  2. Privileged containers with tc available"
    log_warn "Run: ./chaos.sh install-chaos-mesh"
}

# Continuous chaos - kill random pods at intervals
cmd_chaos_loop() {
    local interval=${1:-30}
    local duration=${2:-300}
    local end_time=$((SECONDS + duration))

    log_info "Starting chaos loop: killing random pods every ${interval}s for ${duration}s"
    log_info "Press Ctrl+C to stop"

    while [ $SECONDS -lt $end_time ]; do
        cmd_kill_pod
        sleep "$interval"
    done

    log_success "Chaos loop completed"
}

# Simulate a partial outage - kill half the pods
cmd_partial_outage() {
    local service=${1:-$(get_random_service)}
    local pods=$(kubectl get pods -n "$NAMESPACE" -l "app=$service" --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}')
    local pod_array=($pods)
    local count=${#pod_array[@]}
    local kill_count=$((count / 2))

    if [ $kill_count -lt 1 ]; then
        log_warn "Not enough pods to create partial outage for $service"
        return 1
    fi

    log_info "Killing $kill_count of $count pods for $service"

    for ((i=0; i<kill_count; i++)); do
        kubectl delete pod "${pod_array[$i]}" -n "$NAMESPACE" --grace-period=0 --force &
    done
    wait

    log_success "Partial outage created for $service"
}

# DNS failure simulation - update service selector to non-existent
cmd_dns_blackhole() {
    local service=${1:-$(get_random_service)}
    local duration=${2:-30}

    log_info "Creating DNS blackhole for $service for ${duration}s"

    # Save original selector
    local original_selector=$(kubectl get svc "$service" -n "$NAMESPACE" -o jsonpath='{.spec.selector}')

    # Update to non-existent selector
    kubectl patch svc "$service" -n "$NAMESPACE" -p '{"spec":{"selector":{"app":"nonexistent-chaos-test"}}}'

    log_warn "Service $service now points to no pods"
    sleep "$duration"

    # Restore original
    kubectl patch svc "$service" -n "$NAMESPACE" -p "{\"spec\":{\"selector\":{\"app\":\"$service\"}}}"

    log_success "DNS blackhole removed for $service"
}

# Resource exhaustion - set very low resource limits temporarily
cmd_resource_pressure() {
    local service=${1:-$(get_random_service)}
    local duration=${2:-60}

    log_info "Setting resource pressure on $service for ${duration}s"
    log_warn "This will cause pod restarts!"

    # Patch with very low memory limit
    kubectl patch deployment "$service" -n "$NAMESPACE" -p '{"spec":{"template":{"spec":{"containers":[{"name":"'"$service"'","resources":{"limits":{"memory":"16Mi"}}}]}}}}'

    sleep "$duration"

    # Restore reasonable limits
    kubectl patch deployment "$service" -n "$NAMESPACE" -p '{"spec":{"template":{"spec":{"containers":[{"name":"'"$service"'","resources":{"limits":{"memory":"128Mi"}}}]}}}}'

    log_success "Resource pressure removed from $service"
}

# Rapid scaling chaos
cmd_scale_chaos() {
    local service=${1:-$(get_random_service)}
    local duration=${2:-60}
    local end_time=$((SECONDS + duration))

    log_info "Starting scale chaos on $service for ${duration}s"

    while [ $SECONDS -lt $end_time ]; do
        local replicas=$((RANDOM % 5 + 1))
        log_info "Scaling $service to $replicas"
        kubectl scale deployment "$service" -n "$NAMESPACE" --replicas="$replicas" --timeout=5s || true
        sleep $((RANDOM % 10 + 5))
    done

    # Restore to 2 replicas
    kubectl scale deployment "$service" -n "$NAMESPACE" --replicas=2
    log_success "Scale chaos completed for $service"
}

# Combined chaos - multiple failure types
cmd_full_chaos() {
    local duration=${1:-120}

    log_info "Starting full chaos mode for ${duration}s"
    log_warn "This will cause significant disruption!"

    local end_time=$((SECONDS + duration))

    while [ $SECONDS -lt $end_time ]; do
        local chaos_type=$((RANDOM % 4))

        case $chaos_type in
            0) cmd_kill_pod ;;
            1) cmd_partial_outage ;;
            2)
                local svc=$(get_random_service)
                local replicas=$((RANDOM % 4 + 1))
                kubectl scale deployment "$svc" -n "$NAMESPACE" --replicas="$replicas" --timeout=5s || true
                ;;
            3) cmd_rolling_restart ;;
        esac

        sleep $((RANDOM % 15 + 5))
    done

    # Restore all services to 2 replicas
    for svc in "${SERVICES[@]}"; do
        kubectl scale deployment "$svc" -n "$NAMESPACE" --replicas=2 --timeout=10s || true
    done

    log_success "Full chaos completed"
}

# ==================== TRAFFIC COMMANDS ====================

# Auth token for authenticated requests
AUTH_TOKEN=""

# Get auth token
get_auth_token() {
    if [ -n "$AUTH_TOKEN" ]; then
        return
    fi

    local response=$(curl -s -X POST "$PROXY_URL/auth/login" \
        -H "Content-Type: application/json" \
        -d '{"username":"chaos-tester","password":"password123"}' 2>/dev/null)

    AUTH_TOKEN=$(echo "$response" | grep -o '"token":"[^"]*"' | cut -d'"' -f4)

    if [ -z "$AUTH_TOKEN" ]; then
        AUTH_TOKEN="token-default"
    fi

    log_info "Got auth token: ${AUTH_TOKEN:0:20}..."
}

# Send request to an endpoint
send_request() {
    local method=$1
    local endpoint=$2
    local body=$3
    local need_auth=$4

    local auth_header=""
    if [ "$need_auth" = "true" ]; then
        get_auth_token
        auth_header="-H \"Authorization: Bearer $AUTH_TOKEN\""
    fi

    local cmd="curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 10"
    cmd="$cmd -X $method"
    cmd="$cmd -H 'Content-Type: application/json'"

    if [ "$need_auth" = "true" ]; then
        cmd="$cmd -H 'Authorization: Bearer $AUTH_TOKEN'"
    fi

    if [ -n "$body" ]; then
        cmd="$cmd -d '$body'"
    fi

    cmd="$cmd '$PROXY_URL$endpoint'"

    eval $cmd 2>/dev/null || echo "000"
}

# Generate traffic to the proxy (legacy simple version)
cmd_traffic() {
    local requests=${1:-100}
    local concurrency=${2:-10}
    local endpoint=${3:-"/auth/health"}

    log_info "Generating $requests requests to $PROXY_URL$endpoint (concurrency: $concurrency)"

    if command -v hey &> /dev/null; then
        hey -n "$requests" -c "$concurrency" "$PROXY_URL$endpoint"
    elif command -v ab &> /dev/null; then
        ab -n "$requests" -c "$concurrency" "$PROXY_URL$endpoint"
    elif command -v curl &> /dev/null; then
        for ((i=0; i<requests; i++)); do
            curl -s -o /dev/null -w "%{http_code}\n" "$PROXY_URL$endpoint" &
            [ $((i % concurrency)) -eq 0 ] && wait
        done
        wait
        log_success "Traffic generation complete"
    else
        log_error "No load testing tool found. Install 'hey' or 'ab'"
        return 1
    fi
}

# Comprehensive traffic - hits all endpoints
cmd_traffic_full() {
    local rps=${1:-20}
    local duration=${2:-300}

    log_info "Starting comprehensive traffic at ~$rps req/s for ${duration}s"
    log_info "Testing ALL endpoints including service-to-service calls"

    get_auth_token

    local delay=$(echo "scale=3; 1/$rps" | bc 2>/dev/null || echo "0.05")
    local end_time=$((SECONDS + duration))
    local count=0
    local success=0
    local fail=0

    # Endpoint definitions: "weight:method:path:body:need_auth"
    local endpoints=(
        "10:GET:/auth/health::false"
        "10:GET:/users/health::false"
        "10:GET:/payments/health::false"
        "10:GET:/analytics/health::false"
        "15:POST:/auth/login:{\"username\":\"user$RANDOM\",\"password\":\"pass\"}:false"
        "20:POST:/auth/validate:{}:true"
        "25:GET:/users/profile::true"
        "15:GET:/users/user-default::true"
        "10:POST:/users/activity:{\"user_id\":\"user-$RANDOM\",\"activity\":\"click\"}:false"
        "20:GET:/payments/balance::true"
        "15:POST:/payments/process:{\"amount\":$((RANDOM % 50 + 1))}:true"
        "5:POST:/payments/webhook:{\"event\":\"received\",\"amount\":$RANDOM}:false"
        "15:POST:/analytics/event:{\"type\":\"page_view\",\"user_id\":\"user-$RANDOM\"}:false"
        "10:GET:/analytics/events?limit=10::false"
        "10:GET:/analytics/stats::false"
    )

    # Calculate total weight
    local total_weight=0
    for ep in "${endpoints[@]}"; do
        local weight=$(echo "$ep" | cut -d: -f1)
        total_weight=$((total_weight + weight))
    done

    while [ $SECONDS -lt $end_time ]; do
        # Select random endpoint based on weight
        local rand=$((RANDOM % total_weight))
        local cumulative=0
        local selected=""

        for ep in "${endpoints[@]}"; do
            local weight=$(echo "$ep" | cut -d: -f1)
            cumulative=$((cumulative + weight))
            if [ $rand -lt $cumulative ]; then
                selected="$ep"
                break
            fi
        done

        # Parse selected endpoint
        local method=$(echo "$selected" | cut -d: -f2)
        local path=$(echo "$selected" | cut -d: -f3)
        local body=$(echo "$selected" | cut -d: -f4)
        local need_auth=$(echo "$selected" | cut -d: -f5)

        # Replace $RANDOM in body with actual random
        body=$(echo "$body" | sed "s/\$RANDOM/$RANDOM/g")

        # Send request in background
        (
            local status=$(send_request "$method" "$path" "$body" "$need_auth")
            if [ "$status" -ge 200 ] && [ "$status" -lt 400 ]; then
                echo "OK"
            else
                echo "FAIL:$path:$status"
            fi
        ) &

        ((count++))

        # Collect results periodically
        if [ $((count % 50)) -eq 0 ]; then
            wait
            echo -e "\r${BLUE}Progress: $count requests sent${NC}"
        fi

        sleep "$delay" 2>/dev/null || sleep 0.05
    done

    wait
    echo ""
    log_success "Comprehensive traffic complete: $count requests sent"
}

# Cascade traffic - focuses on service-to-service calls
cmd_traffic_cascade() {
    local rps=${1:-10}
    local duration=${2:-300}

    log_info "Starting CASCADE traffic at ~$rps req/s for ${duration}s"
    log_info "Focus: payments/process (calls auth->users->analytics)"

    get_auth_token

    local delay=$(echo "scale=3; 1/$rps" | bc 2>/dev/null || echo "0.1")
    local end_time=$((SECONDS + duration))
    local count=0

    # Only cascade endpoints
    local endpoints=(
        "40:POST:/payments/process:{\"amount\":$((RANDOM % 50 + 1))}:true"
        "30:GET:/users/profile::true"
        "30:GET:/payments/balance::true"
    )

    local total_weight=100

    while [ $SECONDS -lt $end_time ]; do
        local rand=$((RANDOM % total_weight))
        local selected=""

        if [ $rand -lt 40 ]; then
            selected="POST:/payments/process:{\"amount\":$((RANDOM % 50 + 1))}:true"
        elif [ $rand -lt 70 ]; then
            selected="GET:/users/profile::true"
        else
            selected="GET:/payments/balance::true"
        fi

        local method=$(echo "$selected" | cut -d: -f1)
        local path=$(echo "$selected" | cut -d: -f2)
        local body=$(echo "$selected" | cut -d: -f3)
        local need_auth=$(echo "$selected" | cut -d: -f4)

        (send_request "$method" "$path" "$body" "$need_auth" > /dev/null) &

        ((count++))

        [ $((count % 20)) -eq 0 ] && wait && echo -ne "\r${BLUE}Cascade requests: $count${NC}"

        sleep "$delay" 2>/dev/null || sleep 0.1
    done

    wait
    echo ""
    log_success "Cascade traffic complete: $count requests"
}

# Continuous traffic generator (health endpoints only - legacy)
cmd_traffic_loop() {
    local rps=${1:-10}
    local duration=${2:-300}

    log_info "Starting health-check traffic at ~$rps req/s for ${duration}s"
    log_info "Tip: Use 'traffic-full' for comprehensive endpoint testing"

    local delay=$(echo "scale=3; 1/$rps" | bc 2>/dev/null || echo "0.1")
    local end_time=$((SECONDS + duration))
    local count=0
    local success=0
    local fail=0

    while [ $SECONDS -lt $end_time ]; do
        local endpoint="/${SERVICES[$RANDOM % ${#SERVICES[@]}]}/health"
        local status=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 2 --max-time 5 "$PROXY_URL$endpoint" 2>/dev/null || echo "000")

        ((count++))
        if [ "$status" -ge 200 ] && [ "$status" -lt 300 ]; then
            ((success++))
        else
            ((fail++))
            echo -ne "\r${YELLOW}Request $count: $endpoint -> $status${NC}"
        fi

        [ $((count % 100)) -eq 0 ] && echo -e "\r${BLUE}Progress: $count requests (success: $success, fail: $fail)${NC}"

        sleep "$delay" 2>/dev/null || sleep 0.1
    done

    echo ""
    log_success "Traffic complete: $count total, $success success, $fail failed"
}

# ==================== MONITORING COMMANDS ====================

# Show current state of all services
cmd_status() {
    log_info "Service Status in namespace: $NAMESPACE"
    echo ""

    for svc in "${SERVICES[@]}"; do
        local ready=$(kubectl get deployment "$svc" -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
        local desired=$(kubectl get deployment "$svc" -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "?")
        local status="${GREEN}OK${NC}"

        if [ "$ready" != "$desired" ] || [ "$ready" == "0" ]; then
            status="${RED}DEGRADED${NC}"
        fi

        echo -e "  $svc: $ready/$desired replicas - $status"
    done

    echo ""
    kubectl get pods -n "$NAMESPACE" -o wide
}

# Watch pods continuously
cmd_watch() {
    kubectl get pods -n "$NAMESPACE" -w
}

# ==================== SETUP COMMANDS ====================

# Install Chaos Mesh
cmd_install_chaos_mesh() {
    log_info "Installing Chaos Mesh..."

    # Add Chaos Mesh Helm repo
    helm repo add chaos-mesh https://charts.chaos-mesh.org
    helm repo update

    # Install Chaos Mesh
    kubectl create ns chaos-mesh 2>/dev/null || true
    helm upgrade --install chaos-mesh chaos-mesh/chaos-mesh \
        --namespace chaos-mesh \
        --set chaosDaemon.runtime=containerd \
        --set chaosDaemon.socketPath=/run/containerd/containerd.sock \
        --set dashboard.securityMode=false

    log_success "Chaos Mesh installed!"
    log_info "Access dashboard: kubectl port-forward -n chaos-mesh svc/chaos-dashboard 2333:2333"
    log_info "Then open: http://localhost:2333"
}

# Reset all services to normal
cmd_reset() {
    log_info "Resetting all services to normal state..."

    for svc in "${SERVICES[@]}"; do
        kubectl scale deployment "$svc" -n "$NAMESPACE" --replicas=2 --timeout=30s || true
    done

    log_success "All services reset to 2 replicas"
}

# ==================== HELP ====================

show_help() {
    cat << 'EOF'
Ananse Chaos Testing Script

USAGE:
    ./chaos.sh <command> [options]

CHAOS COMMANDS:
    kill-pod [service]              Kill a random pod (or from specific service)
    kill-service [service]          Kill all pods of a service
    scale-bounce [svc] [rep] [sec]  Scale to 0 then back up
    scale-up [service] [replicas]   Scale up a service
    scale-down [service] [replicas] Scale down a service
    rolling-restart [service]       Trigger rolling restart
    partial-outage [service]        Kill half the pods
    dns-blackhole [svc] [seconds]   Temporarily break DNS
    resource-pressure [svc] [sec]   Apply memory pressure
    scale-chaos [service] [sec]     Random scaling chaos
    chaos-loop [interval] [dur]     Kill random pods continuously
    full-chaos [duration]           Combined chaos types

TRAFFIC COMMANDS:
    traffic [n] [c] [endpoint]      Generate n requests with c concurrency
    traffic-loop [rps] [duration]   Continuous health-check traffic
    traffic-full [rps] [duration]   Comprehensive traffic (all endpoints)
    traffic-cascade [rps] [dur]     Cascade traffic (service-to-service)

MONITORING COMMANDS:
    status                          Show current service status
    watch                           Watch pods continuously
    reset                           Reset all services to normal

SETUP COMMANDS:
    install-chaos-mesh              Install Chaos Mesh for advanced chaos

ENVIRONMENT VARIABLES:
    NAMESPACE                       Kubernetes namespace (default: ananse)
    PROXY_URL                       Proxy URL (default: http://localhost:8089)

EXAMPLES:
    # Kill a random pod
    ./chaos.sh kill-pod

    # Kill specific service's pod
    ./chaos.sh kill-pod auth

    # Scale bounce with 30s downtime
    ./chaos.sh scale-bounce users 2 30

    # Continuous chaos for 5 minutes
    ./chaos.sh chaos-loop 20 300

    # Generate traffic while running chaos
    ./chaos.sh traffic-loop 50 300 &
    ./chaos.sh chaos-loop 30 300

    # Full chaos mode
    ./chaos.sh full-chaos 120
EOF
}

# ==================== MAIN ====================

main() {
    local command="${1:-help}"
    shift || true

    case "$command" in
        kill-pod)           check_prereqs && cmd_kill_pod "$@" ;;
        kill-service)       check_prereqs && cmd_kill_service "$@" ;;
        scale-bounce)       check_prereqs && cmd_scale_bounce "$@" ;;
        scale-up)           check_prereqs && cmd_scale_up "$@" ;;
        scale-down)         check_prereqs && cmd_scale_down "$@" ;;
        rolling-restart)    check_prereqs && cmd_rolling_restart "$@" ;;
        partial-outage)     check_prereqs && cmd_partial_outage "$@" ;;
        dns-blackhole)      check_prereqs && cmd_dns_blackhole "$@" ;;
        resource-pressure)  check_prereqs && cmd_resource_pressure "$@" ;;
        scale-chaos)        check_prereqs && cmd_scale_chaos "$@" ;;
        chaos-loop)         check_prereqs && cmd_chaos_loop "$@" ;;
        full-chaos)         check_prereqs && cmd_full_chaos "$@" ;;
        traffic)            cmd_traffic "$@" ;;
        traffic-loop)       cmd_traffic_loop "$@" ;;
        traffic-full)       cmd_traffic_full "$@" ;;
        traffic-cascade)    cmd_traffic_cascade "$@" ;;
        status)             check_prereqs && cmd_status ;;
        watch)              check_prereqs && cmd_watch ;;
        reset)              check_prereqs && cmd_reset ;;
        install-chaos-mesh) cmd_install_chaos_mesh ;;
        help|--help|-h)     show_help ;;
        *)
            log_error "Unknown command: $command"
            show_help
            exit 1
            ;;
    esac
}

main "$@"