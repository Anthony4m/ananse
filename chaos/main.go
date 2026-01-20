package main

import (
	"context"
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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

type ChaosRunner struct {
	clientset *kubernetes.Clientset
	namespace string
	services  []string
	proxyURL  string

	// Traffic stats
	totalRequests   int64
	successRequests int64
	failedRequests  int64

	// Control
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type ChaosConfig struct {
	Namespace     string
	ProxyURL      string
	Duration      time.Duration
	TrafficRPS    int
	ChaosInterval time.Duration
	EnablePodKill bool
	EnableScaling bool
	EnableRolling bool
	Verbose       bool
}

func NewChaosRunner(cfg ChaosConfig) (*ChaosRunner, error) {
	config, err := loadKubeConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &ChaosRunner{
		clientset: clientset,
		namespace: cfg.Namespace,
		services:  []string{"auth", "users", "payments", "analytics"},
		proxyURL:  cfg.ProxyURL,
		ctx:       ctx,
		cancel:    cancel,
	}, nil
}

func loadKubeConfig() (*rest.Config, error) {
	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}

	// Fall back to kubeconfig
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = home + "/.kube/config"
		}
	}

	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func (c *ChaosRunner) randomService() string {
	return c.services[rand.Intn(len(c.services))]
}

// KillRandomPod deletes a random pod from a service
func (c *ChaosRunner) KillRandomPod(service string) error {
	pods, err := c.clientset.CoreV1().Pods(c.namespace).List(c.ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", service),
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return err
	}

	if len(pods.Items) == 0 {
		return fmt.Errorf("no running pods for %s", service)
	}

	pod := pods.Items[rand.Intn(len(pods.Items))]
	fmt.Printf("[CHAOS] Killing pod: %s (service: %s)\n", pod.Name, service)

	gracePeriod := int64(0)
	return c.clientset.CoreV1().Pods(c.namespace).Delete(c.ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
	})
}

// ScaleDeployment scales a deployment to the specified replicas
func (c *ChaosRunner) ScaleDeployment(service string, replicas int32) error {
	scale, err := c.clientset.AppsV1().Deployments(c.namespace).GetScale(c.ctx, service, metav1.GetOptions{})
	if err != nil {
		return err
	}

	scale.Spec.Replicas = replicas
	_, err = c.clientset.AppsV1().Deployments(c.namespace).UpdateScale(c.ctx, service, scale, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	fmt.Printf("[CHAOS] Scaled %s to %d replicas\n", service, replicas)
	return nil
}

// RollingRestart triggers a rolling restart
func (c *ChaosRunner) RollingRestart(service string) error {
	deployment, err := c.clientset.AppsV1().Deployments(c.namespace).Get(c.ctx, service, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}
	deployment.Spec.Template.Annotations["chaos.ananse.io/restartedAt"] = time.Now().Format(time.RFC3339)

	_, err = c.clientset.AppsV1().Deployments(c.namespace).Update(c.ctx, deployment, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	fmt.Printf("[CHAOS] Rolling restart: %s\n", service)
	return nil
}

// SendRequest sends a request through the proxy
func (c *ChaosRunner) SendRequest(endpoint string) (int, time.Duration, error) {
	start := time.Now()

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Get(c.proxyURL + endpoint)
	duration := time.Since(start)

	if err != nil {
		return 0, duration, err
	}
	defer resp.Body.Close()

	return resp.StatusCode, duration, nil
}

// TrafficGenerator generates continuous traffic
func (c *ChaosRunner) TrafficGenerator(rps int) {
	c.wg.Add(1)
	defer c.wg.Done()

	if rps <= 0 {
		return
	}

	ticker := time.NewTicker(time.Second / time.Duration(rps))
	defer ticker.Stop()

	endpoints := []string{"/auth/health", "/users/health", "/payments/health", "/analytics/health"}

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			endpoint := endpoints[rand.Intn(len(endpoints))]

			go func(ep string) {
				atomic.AddInt64(&c.totalRequests, 1)

				status, latency, err := c.SendRequest(ep)
				if err != nil || status >= 400 {
					atomic.AddInt64(&c.failedRequests, 1)
					if err != nil {
						fmt.Printf("[TRAFFIC] %s -> ERROR: %v\n", ep, err)
					} else {
						fmt.Printf("[TRAFFIC] %s -> %d (%v)\n", ep, status, latency)
					}
				} else {
					atomic.AddInt64(&c.successRequests, 1)
				}
			}(endpoint)
		}
	}
}

// ChaosLoop runs continuous chaos
func (c *ChaosRunner) ChaosLoop(interval time.Duration, cfg ChaosConfig) {
	c.wg.Add(1)
	defer c.wg.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	chaosActions := []func(){
		func() {
			if cfg.EnablePodKill {
				svc := c.randomService()
				if err := c.KillRandomPod(svc); err != nil {
					fmt.Printf("[CHAOS] Error killing pod: %v\n", err)
				}
			}
		},
		func() {
			if cfg.EnableScaling {
				svc := c.randomService()
				replicas := int32(rand.Intn(4) + 1)
				if err := c.ScaleDeployment(svc, replicas); err != nil {
					fmt.Printf("[CHAOS] Error scaling: %v\n", err)
				}
			}
		},
		func() {
			if cfg.EnableRolling {
				svc := c.randomService()
				if err := c.RollingRestart(svc); err != nil {
					fmt.Printf("[CHAOS] Error rolling restart: %v\n", err)
				}
			}
		},
	}

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			action := chaosActions[rand.Intn(len(chaosActions))]
			action()
		}
	}
}

// PrintStats prints traffic statistics
func (c *ChaosRunner) PrintStats() {
	total := atomic.LoadInt64(&c.totalRequests)
	success := atomic.LoadInt64(&c.successRequests)
	failed := atomic.LoadInt64(&c.failedRequests)

	successRate := float64(0)
	if total > 0 {
		successRate = float64(success) / float64(total) * 100
	}

	fmt.Printf("\n=== Traffic Statistics ===\n")
	fmt.Printf("Total Requests:  %d\n", total)
	fmt.Printf("Successful:      %d\n", success)
	fmt.Printf("Failed:          %d\n", failed)
	fmt.Printf("Success Rate:    %.2f%%\n", successRate)
}

// Reset resets all services to normal state
func (c *ChaosRunner) Reset() {
	fmt.Println("[RESET] Resetting all services to 2 replicas...")
	for _, svc := range c.services {
		if err := c.ScaleDeployment(svc, 2); err != nil {
			fmt.Printf("[RESET] Error resetting %s: %v\n", svc, err)
		}
	}
}

// Run starts the chaos testing
func (c *ChaosRunner) Run(cfg ChaosConfig) {
	fmt.Println("=== Ananse Chaos Testing ===")
	fmt.Printf("Namespace:      %s\n", cfg.Namespace)
	fmt.Printf("Duration:       %v\n", cfg.Duration)
	fmt.Printf("Traffic RPS:    %d\n", cfg.TrafficRPS)
	fmt.Printf("Chaos Interval: %v\n", cfg.ChaosInterval)
	fmt.Printf("Pod Kill:       %v\n", cfg.EnablePodKill)
	fmt.Printf("Scaling:        %v\n", cfg.EnableScaling)
	fmt.Printf("Rolling:        %v\n", cfg.EnableRolling)
	fmt.Println("=============================")

	// Start traffic generator
	go c.TrafficGenerator(cfg.TrafficRPS)

	// Start chaos loop
	go c.ChaosLoop(cfg.ChaosInterval, cfg)

	// Stats printer
	statsTicker := time.NewTicker(30 * time.Second)
	go func() {
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-statsTicker.C:
				c.PrintStats()
			}
		}
	}()

	// Wait for duration or interrupt
	select {
	case <-time.After(cfg.Duration):
		fmt.Println("\n[INFO] Duration reached, stopping...")
	case <-c.ctx.Done():
		fmt.Println("\n[INFO] Interrupted, stopping...")
	}

	c.cancel()
	c.wg.Wait()
	statsTicker.Stop()

	c.PrintStats()
	c.Reset()
}

func main() {
	cfg := ChaosConfig{}

	flag.StringVar(&cfg.Namespace, "namespace", "ananse", "Kubernetes namespace")
	flag.StringVar(&cfg.ProxyURL, "proxy-url", "http://localhost:8089", "Proxy URL")
	flag.DurationVar(&cfg.Duration, "duration", 5*time.Minute, "Test duration")
	flag.IntVar(&cfg.TrafficRPS, "rps", 50, "Traffic requests per second")
	flag.DurationVar(&cfg.ChaosInterval, "interval", 30*time.Second, "Chaos action interval")
	flag.BoolVar(&cfg.EnablePodKill, "pod-kill", true, "Enable pod killing")
	flag.BoolVar(&cfg.EnableScaling, "scaling", true, "Enable random scaling")
	flag.BoolVar(&cfg.EnableRolling, "rolling", false, "Enable rolling restarts")
	flag.BoolVar(&cfg.Verbose, "verbose", false, "Verbose output")
	flag.Parse()

	runner, err := NewChaosRunner(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Handle interrupt
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nReceived interrupt signal...")
		runner.cancel()
	}()

	runner.Run(cfg)
}
