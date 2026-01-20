#!/bin/bash
set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

print_step() { echo -e "${BLUE}▶${NC} $1"; }
print_success() { echo -e "${GREEN}✅${NC} $1"; }
print_warning() { echo -e "${YELLOW}⚠️${NC} $1"; }
print_error() { echo -e "${RED}❌${NC} $1"; }

# --- 1. CLUSTER CHECK ---
print_step "Checking for Kind cluster..."
CLUSTER_NAME=$(kind get clusters 2>/dev/null | head -n 1)

if [ -z "$CLUSTER_NAME" ]; then
    print_error "No Kind cluster found running."
    echo "   Please run: 'kind create cluster'"
    exit 1
fi

print_success "Detected Kind cluster: '$CLUSTER_NAME'"
KCTX="--context kind-$CLUSTER_NAME"

# Verify kubectl context
if ! kubectl cluster-info $KCTX > /dev/null 2>&1; then
    print_warning "Could not switch context automatically. Using current context..."
    KCTX=""
fi

# --- 2. PARSE ARGUMENTS ---
SKIP_INFRA=false
SKIP_BUILD=false
SERVICES=""

while [[ $# -gt 0 ]]; do
    case $1 in
        --skip-infra)
            SKIP_INFRA=true
            shift
            ;;
        --skip-build)
            SKIP_BUILD=true
            shift
            ;;
        *)
            SERVICES="$SERVICES $1"
            shift
            ;;
    esac
done

SERVICES=$(echo $SERVICES | xargs) # trim whitespace

if [ -z "$SERVICES" ]; then
    SERVICES="analytics auth echo payments users proxy controlplane"
    MODE="all"
else
    MODE="selected"
fi

echo ""
echo "================================================"
echo "   Ananse Build & Deploy Script"
echo "================================================"
echo "  Cluster:    $CLUSTER_NAME"
echo "  Mode:       $MODE"
echo "  Services:   $SERVICES"
echo "  Skip Infra: $SKIP_INFRA"
echo "  Skip Build: $SKIP_BUILD"
echo "================================================"
echo ""

# --- 3. APPLY INFRASTRUCTURE (namespace, databases) ---
if [ "$SKIP_INFRA" = false ] && [ "$MODE" = "all" ]; then
    print_step "Setting up infrastructure..."

    # Create namespace
    echo "   Creating namespace..."
    kubectl $KCTX apply -f k8s/namespace.yaml

    # Apply database infrastructure
    echo "   Applying database configurations..."
    kubectl $KCTX apply -f k8s/db-init-configmap.yaml
    kubectl $KCTX apply -f k8s/postgres.yaml
    kubectl $KCTX apply -f k8s/redis.yaml

    # Apply other configs
    if [ -f "k8s/configmap.yaml" ]; then
        kubectl $KCTX apply -f k8s/configmap.yaml
    fi
    if [ -f "k8s/controlplane-rbac.yaml" ]; then
        kubectl $KCTX apply -f k8s/controlplane-rbac.yaml
    fi

    print_success "Infrastructure manifests applied"

    # Wait for databases to be ready
    print_step "Waiting for databases to be ready..."
    echo "   Waiting for PostgreSQL..."
    kubectl $KCTX wait --for=condition=ready pod -l app=postgres -n ananse --timeout=120s 2>/dev/null || {
        print_warning "PostgreSQL not ready yet, continuing anyway..."
    }

    echo "   Waiting for Redis..."
    kubectl $KCTX wait --for=condition=ready pod -l app=redis -n ananse --timeout=60s 2>/dev/null || {
        print_warning "Redis not ready yet, continuing anyway..."
    }

    print_success "Database infrastructure ready"
fi

# --- 4. BUILD & LOAD IMAGES ---
if [ "$SKIP_BUILD" = false ]; then
    print_step "Building and loading Docker images..."

    for SERVICE in $SERVICES; do
        echo "------------------------------------------------"
        echo "📦 Building $SERVICE:latest..."

        # Build image
        if [ "$SERVICE" == "proxy" ]; then
            docker build -t $SERVICE:latest -f proxy/Dockerfile . --quiet
        elif [ "$SERVICE" == "controlplane" ]; then
            docker build -t $SERVICE:latest -f controlplane/Dockerfile . --quiet
        else
            docker build -t $SERVICE:latest -f services/$SERVICE/Dockerfile . --quiet
        fi

        echo "🚚 Loading $SERVICE:latest into cluster..."
        kind load docker-image $SERVICE:latest --name "$CLUSTER_NAME"
    done

    print_success "All images built and loaded"
fi

# --- 5. APPLY SERVICE MANIFESTS ---
if [ "$MODE" = "all" ]; then
    print_step "Applying service manifests..."

    # Apply all service yamls
    for yaml in k8s/auth.yaml k8s/users.yaml k8s/payments.yaml k8s/analytics.yaml k8s/echo.yaml k8s/proxy-gateway.yaml; do
        if [ -f "$yaml" ]; then
            echo "   Applying $yaml..."
            kubectl $KCTX apply -f "$yaml"
        fi
    done

    print_success "Service manifests applied"
fi

# --- 6. RESTART PODS ---
print_step "Restarting pods..."

# Ensure namespace exists
if ! kubectl $KCTX get namespace ananse > /dev/null 2>&1; then
    print_warning "Namespace 'ananse' not found. Skipping restart."
    exit 0
fi

if [ "$MODE" == "all" ]; then
    # Rolling restart for all deployments (graceful)
    kubectl $KCTX rollout restart deployment -n ananse 2>/dev/null || {
        # Fallback: delete pods
        kubectl $KCTX delete pods --all -n ananse --ignore-not-found
    }
else
    for SERVICE in $SERVICES; do
        if [[ "$SERVICE" == "proxy" || "$SERVICE" == "controlplane" ]]; then
            echo "   Restarting proxy-gateway..."
            kubectl $KCTX rollout restart deployment/proxy-gateway -n ananse 2>/dev/null || \
                kubectl $KCTX delete pods -l app=proxy-gateway -n ananse --ignore-not-found
        else
            echo "   Restarting $SERVICE..."
            kubectl $KCTX rollout restart deployment/$SERVICE -n ananse 2>/dev/null || \
                kubectl $KCTX delete pods -l app=$SERVICE -n ananse --ignore-not-found
        fi
    done
fi

# --- 7. WAIT FOR PODS ---
print_step "Waiting for pods to be ready..."
sleep 3

kubectl $KCTX get pods -n ananse

echo ""
echo "================================================"
print_success "Deployment complete!"
echo "================================================"
echo ""
echo "Useful commands:"
echo "  kubectl get pods -n ananse              # Check pod status"
echo "  kubectl logs -f deploy/auth -n ananse   # View auth logs"
echo "  kubectl port-forward svc/proxy 8089:8089 -n ananse  # Access proxy"
echo ""
echo "To run traffic generator:"
echo "  kubectl port-forward svc/proxy 8089:8089 -n ananse &"
echo "  cd chaos/traffic && go run . -rps 50 -duration 5m"
echo ""



 - APPLICATION_BALANCE_REPORT_EMAIL_RECIPIENTS=anthony@blacktechnologies.io,jones@blacktechnologies.io,kesaobaka@blacktechnologies.io,finance@blacktechnologies.io,leroy@blacktechnologies.io,lesetse@blacktechnologies.io