package controlplane

import (
	pb "ananse/controlplane/cmd/configpb"
	px "ananse/pkg/proxy"
	"context"
	"fmt"
	"path/filepath"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const (
	resyncPeriod   = 10 * time.Minute
	debounceWindow = 500 * time.Millisecond
	defaultPort    = int32(8080)
)

type K8sClient struct {
	clientset kubernetes.Interface
	factory   informers.SharedInformerFactory
	namespace string

	serviceInformer cache.SharedIndexInformer
	sliceInformer   cache.SharedIndexInformer

	configChan chan *pb.Config
	updateChan chan struct{}
}

func NewK8sClient(namespace string) (*K8sClient, error) {
	if px.Logger == nil {
		px.InitLogger()
	}

	cfg, err := loadKubeConfig()
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}

	factory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		resyncPeriod,
		informers.WithNamespace(namespace),
	)

	return &K8sClient{
		clientset:       clientset,
		factory:         factory,
		namespace:       namespace,
		serviceInformer: factory.Core().V1().Services().Informer(),
		sliceInformer:   factory.Discovery().V1().EndpointSlices().Informer(),
		configChan:      make(chan *pb.Config, 5),
		updateChan:      make(chan struct{}, 1),
	}, nil
}

func loadKubeConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}

	kubeconfig := filepath.Join(homedir.HomeDir(), ".kube", "config")
	cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return cfg, nil
}

func (k *K8sClient) Watch(ctx context.Context) <-chan *pb.Config {
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { k.signalUpdate() },
		UpdateFunc: func(_, _ any) { k.signalUpdate() },
		DeleteFunc: func(any) { k.signalUpdate() },
	}

	k.serviceInformer.AddEventHandler(handler)
	k.sliceInformer.AddEventHandler(handler)

	k.factory.Start(ctx.Done())

	if !cache.WaitForCacheSync(
		ctx.Done(),
		k.serviceInformer.HasSynced,
		k.sliceInformer.HasSynced,
	) {
		px.Logger.Fatal("Failed to sync K8s caches")
	}

	px.Logger.Info("K8s cache synced")

	go k.runDebouncer(ctx)

	// Emit initial config
	if cfg := k.buildConfig(); k.validate(cfg) == nil {
		k.configChan <- cfg
	}

	return k.configChan
}

func (k *K8sClient) signalUpdate() {
	select {
	case k.updateChan <- struct{}{}:
	default:
	}
}

func (k *K8sClient) runDebouncer(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()

	for {
		select {
		case <-k.updateChan:
			timer.Reset(debounceWindow)

		case <-timer.C:
			cfg := k.buildConfig()
			if err := k.validate(cfg); err != nil {
				px.Logger.Warn("Invalid config", zap.Error(err))
				continue
			}
			select {
			case k.configChan <- cfg:
				px.Logger.Info("Config updated",
					zap.Int("services", len(cfg.Services)))
			default:
				px.Logger.Warn("Config channel full")
			}

		case <-ctx.Done():
			close(k.configChan)
			return
		}
	}
}

func (k *K8sClient) buildConfig() *pb.Config {
	cfg := &pb.Config{
		Version: fmt.Sprintf("k8s-%d", time.Now().Unix()),
		ProxyConfig: &pb.ProxyConfig{
			Port:                       8089,
			MetricsPort:                9090,
			HealthCheckIntervalSeconds: 10,
		},
	}

	for _, obj := range k.serviceInformer.GetStore().List() {
		svc, ok := obj.(*corev1.Service)
		if !ok || len(svc.Spec.Selector) == 0 {
			continue
		}

		// Only include services with mesh label
		if svc.Labels["ananse.io/mesh"] != "enabled" {
			continue
		}

		endpoints := k.buildEndpoints(svc.Namespace, svc.Name)
		if len(endpoints) == 0 {
			continue
		}

		cfg.Services = append(cfg.Services, &pb.Service{
			Name:      svc.Name,
			Endpoints: endpoints,
			Routes: []*pb.Route{
				{
					Path:    "/" + svc.Name,
					Methods: []string{"GET", "POST", "PUT", "DELETE", "PATCH"},
				},
			},
		})
	}

	return cfg
}

func (k *K8sClient) buildEndpoints(ns, svcName string) []*pb.Endpoint {
	var out []*pb.Endpoint

	for _, obj := range k.sliceInformer.GetStore().List() {
		slice, ok := obj.(*discoveryv1.EndpointSlice)
		if !ok ||
			slice.Namespace != ns ||
			slice.Labels["kubernetes.io/service-name"] != svcName {
			continue
		}

		port := defaultPort
		if len(slice.Ports) > 0 && slice.Ports[0].Port != nil {
			port = *slice.Ports[0].Port
		}

		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			for _, addr := range ep.Addresses {
				out = append(out, &pb.Endpoint{
					Address: fmt.Sprintf("%s:%d", addr, port),
				})
			}
		}
	}

	return out
}

func (k *K8sClient) validate(cfg *pb.Config) error {
	if cfg.ProxyConfig == nil {
		return fmt.Errorf("proxy_config is required")
	}
	if len(cfg.Services) == 0 {
		return fmt.Errorf("at least one service is required")
	}
	return nil
}
