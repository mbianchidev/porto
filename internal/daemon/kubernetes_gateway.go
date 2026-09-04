package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/ports"
	"github.com/mbianchidev/porto/internal/store"
)

const kubernetesRouteReconcileInterval = 10 * time.Second

var kubernetesHostnameCharacters = regexp.MustCompile(`[^a-z0-9-]+`)

func (s *Server) kubernetesRouteLoop(ctx context.Context) {
	s.reconcileKubernetesClusters(ctx)
	ticker := time.NewTicker(kubernetesRouteReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileKubernetesClusters(ctx)
		}
	}
}

func (s *Server) reconcileKubernetesClusters(ctx context.Context) {
	settings, err := s.store.Settings(ctx)
	if err != nil {
		log.Printf("read settings for Kubernetes route reconciliation: %v", err)
		return
	}
	if !settings.KubernetesEnabled {
		return
	}
	clusters, err := s.clusters.List(ctx)
	if err != nil {
		log.Printf("list clusters for Kubernetes route reconciliation: %v", err)
		return
	}
	for _, cluster := range clusters {
		if cluster.State != "running" && cluster.State != "degraded" {
			continue
		}
		release, acquired, lockErr := s.tryBeginKubernetesClusterReconcile(ctx, cluster.Name)
		if lockErr != nil {
			log.Printf("lock Kubernetes route reconciliation for %s: %v", cluster.Context, lockErr)
			continue
		}
		if !acquired {
			continue
		}
		s.reconcileKubernetesCluster(ctx, cluster, release)
	}
}

func (s *Server) reconcileKubernetesCluster(
	ctx context.Context,
	cluster kubernetes.Cluster,
	release func(),
) {
	defer release()
	currentClusters, err := s.clusters.List(ctx)
	if err != nil {
		log.Printf("revalidate cluster %s for Kubernetes route reconciliation: %v", cluster.Context, err)
		return
	}
	var current kubernetes.Cluster
	for _, candidate := range currentClusters {
		if candidate.Name == cluster.Name {
			current = candidate
			break
		}
	}
	if current.Name == "" || (current.State != "running" && current.State != "degraded") {
		return
	}
	if err := s.ensureKubernetesClusterAddons(ctx, current); err != nil {
		log.Printf("ensure Kubernetes addons for %s: %v", current.Context, err)
		return
	}
	services, err := s.kubernetes.Services(ctx, current.Context, "")
	if err != nil {
		log.Printf("list Kubernetes services for %s: %v", current.Context, err)
		return
	}
	if err := s.reconcileServiceRoutes(ctx, current.Context, services); err != nil {
		log.Printf("reconcile Kubernetes routes for %s: %v", current.Context, err)
	}
}

func (s *Server) ensureKubernetesClusterAddons(ctx context.Context, cluster kubernetes.Cluster) error {
	s.mu.Lock()
	ready := s.kubeAddons[cluster.Context]
	s.mu.Unlock()
	if ready {
		return nil
	}
	if err := s.clusters.EnsureAddons(ctx, cluster.Name); err != nil {
		return err
	}
	s.mu.Lock()
	if s.kubeAddons == nil {
		s.kubeAddons = make(map[string]bool)
	}
	s.kubeAddons[cluster.Context] = true
	s.mu.Unlock()
	return nil
}

func (s *Server) forgetKubernetesClusterAddons(contextNames ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, contextName := range contextNames {
		delete(s.kubeAddons, contextName)
	}
}

func (s *Server) rememberKubernetesClusterAddons(contextName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kubeAddons == nil {
		s.kubeAddons = make(map[string]bool)
	}
	s.kubeAddons[contextName] = true
}

func (s *Server) managedKubernetesContext(ctx context.Context, contextName string) (bool, error) {
	clusters, err := s.clusters.List(ctx)
	if err != nil {
		return false, fmt.Errorf("list managed Kubernetes clusters: %w", err)
	}
	for _, cluster := range clusters {
		if cluster.Context == contextName {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) reconcileServiceRoutes(
	ctx context.Context,
	contextName string,
	services []kubernetes.Service,
) error {
	if contextName == "" {
		return errors.New("Kubernetes context is required for service routing")
	}
	existing, err := s.store.ListKubernetesRoutes(ctx, contextName)
	if err != nil {
		return fmt.Errorf("list stored Kubernetes routes: %w", err)
	}
	stored := make(map[string]store.KubernetesRoute, len(existing))
	for _, route := range existing {
		stored[kubernetesRouteKey(route.Namespace, route.Service, route.ServicePort)] = route
	}

	desired := make(map[string]bool)
	desiredRaw := make(map[string]bool)
	var routeErrors []error
	routesChanged := false
	var forward *kubeForward
	var forwardErr error
	gatewayResolved := false
	for serviceIndex := range services {
		service := &services[serviceIndex]
		if !routableKubernetesService(*service) {
			continue
		}
		for portIndex := range service.Ports {
			servicePort := &service.Ports[portIndex]
			if servicePort.Protocol != "" && !strings.EqualFold(servicePort.Protocol, "TCP") {
				continue
			}
			if !httpKubernetesServicePort(*servicePort) {
				rawKey := kubernetesRawForwardKey(contextName, service.Namespace, service.Name, servicePort.Port)
				desiredRaw[rawKey] = true
				rawForward, err := s.ensureKubernetesRawForward(ctx, contextName, *service, *servicePort)
				if err != nil {
					routeErrors = append(routeErrors, err)
					servicePort.GatewayError = err.Error()
					continue
				}
				servicePort.LocalPort = rawForward.port
				continue
			}
			if !gatewayResolved {
				forward, forwardErr = s.ensureKubernetesGatewayForward(ctx, contextName)
				gatewayResolved = true
			}
			key := kubernetesRouteKey(service.Namespace, service.Name, servicePort.Port)
			desired[key] = true
			route, ok := stored[key]
			if !ok {
				route, err = s.store.EnsureKubernetesRoute(ctx, store.KubernetesRoute{
					Context:     contextName,
					Namespace:   service.Namespace,
					Service:     service.Name,
					ServicePort: servicePort.Port,
					Hostname:    kubernetesRouteHostname(contextName, service.Namespace, service.Name, servicePort.Port),
				})
				if err != nil {
					routeErrors = append(routeErrors, fmt.Errorf("store route for %s/%s:%d: %w", service.Namespace, service.Name, servicePort.Port, err))
					continue
				}
				stored[key] = route
				routesChanged = true
			}
			if err := s.kubernetes.ApplyHTTPRoute(
				ctx,
				contextName,
				service.Namespace,
				service.Name,
				servicePort.Port,
				route.Hostname,
			); err != nil {
				routeErrors = append(routeErrors, err)
				servicePort.GatewayError = err.Error()
			}
			servicePort.Hostname = route.Hostname
			servicePort.HTTPURL = config.ProjectHTTPURL(route.Hostname)
			servicePort.HTTPSURL = config.ProjectHTTPSURL(route.Hostname)
			servicePort.GatewayReady = forwardErr == nil && forward != nil && servicePort.GatewayError == ""
			if forwardErr != nil {
				servicePort.GatewayError = forwardErr.Error()
			}
		}
	}
	if err := s.stopStaleKubernetesRawForwards(contextName, desiredRaw); err != nil {
		routeErrors = append(routeErrors, err)
	}

	for key, route := range stored {
		if desired[key] {
			continue
		}
		if err := s.kubernetes.DeleteHTTPRoute(
			ctx,
			contextName,
			route.Namespace,
			route.Service,
			route.ServicePort,
		); err != nil {
			routeErrors = append(routeErrors, err)
			continue
		}
		if err := s.store.DeleteKubernetesRoute(
			ctx,
			contextName,
			route.Namespace,
			route.Service,
			route.ServicePort,
		); err != nil {
			routeErrors = append(routeErrors, err)
			continue
		}
		routesChanged = true
	}
	if routesChanged && s.tlsCertificates != nil {
		if _, err := s.ensureProjectCertificate(ctx); err != nil {
			routeErrors = append(routeErrors, fmt.Errorf("refresh local TLS certificate: %w", err))
		}
	}
	return errors.Join(routeErrors...)
}

func (s *Server) decorateServiceRoutes(ctx context.Context, contextName string, services []kubernetes.Service) error {
	for serviceIndex := range services {
		service := &services[serviceIndex]
		if !routableKubernetesService(*service) {
			continue
		}
		for portIndex := range service.Ports {
			servicePort := &service.Ports[portIndex]
			if servicePort.Protocol != "" && !strings.EqualFold(servicePort.Protocol, "TCP") {
				continue
			}
			if !httpKubernetesServicePort(*servicePort) {
				if forward := s.kubernetesForward(kubernetesRawForwardKey(
					contextName,
					service.Namespace,
					service.Name,
					servicePort.Port,
				)); forward != nil {
					servicePort.LocalPort = forward.port
				}
				continue
			}
			route, err := s.store.GetKubernetesRoute(
				ctx,
				contextName,
				service.Namespace,
				service.Name,
				servicePort.Port,
			)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return err
			}
			servicePort.Hostname = route.Hostname
			servicePort.HTTPURL = config.ProjectHTTPURL(route.Hostname)
			servicePort.HTTPSURL = config.ProjectHTTPSURL(route.Hostname)
			servicePort.GatewayReady = s.kubernetesGatewayForward(contextName) != nil
		}
	}
	return nil
}

func routableKubernetesService(service kubernetes.Service) bool {
	if service.ClusterIP == "" || strings.EqualFold(service.ClusterIP, "none") {
		return false
	}
	switch service.Namespace {
	case "kube-system", "kube-public", "kube-node-lease", "envoy-gateway-system", "porto-system", "local-path-storage":
		return false
	default:
		return service.Name != "kubernetes"
	}
}

func httpKubernetesServicePort(port kubernetes.ServicePort) bool {
	appProtocol := strings.ToLower(strings.TrimSpace(port.AppProtocol))
	if appProtocol == "http" || appProtocol == "kubernetes.io/h2c" || appProtocol == "kubernetes.io/ws" {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(port.Name))
	if name == "http" || name == "web" || strings.HasPrefix(name, "http-") || strings.HasPrefix(name, "web-") {
		return true
	}
	switch port.Port {
	case 80, 3000, 4200, 5000, 5173, 5174, 8000, 8080, 8888:
		return true
	default:
		return false
	}
}

func (s *Server) ensureKubernetesGatewayForward(ctx context.Context, contextName string) (*kubeForward, error) {
	key := contextName + "/gateway"
	s.mu.Lock()
	existing := s.kubeForwards[key]
	s.mu.Unlock()
	if existing != nil {
		return existing, nil
	}
	service, err := s.kubernetes.GatewayServiceName(ctx, contextName)
	if err != nil {
		return nil, err
	}
	used, err := s.store.UsedPorts(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	for _, forward := range s.kubeForwards {
		used[forward.port] = true
	}
	s.mu.Unlock()
	localPort, err := ports.Pick(0, 45000, used)
	if err != nil {
		return nil, fmt.Errorf("allocate localhost port for Kubernetes gateway: %w", err)
	}
	return s.startServiceForward(
		key,
		contextName,
		"envoy-gateway-system",
		service,
		localPort,
		80,
	)
}

func (s *Server) ensureKubernetesRawForward(
	ctx context.Context,
	contextName string,
	service kubernetes.Service,
	servicePort kubernetes.ServicePort,
) (*kubeForward, error) {
	key := kubernetesRawForwardKey(contextName, service.Namespace, service.Name, servicePort.Port)
	if existing := s.kubernetesForward(key); existing != nil {
		return existing, nil
	}
	used, err := s.store.UsedPorts(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	for _, forward := range s.kubeForwards {
		used[forward.port] = true
	}
	s.mu.Unlock()
	localPort, err := ports.Pick(int(servicePort.NodePort), 45000, used)
	if err != nil {
		return nil, fmt.Errorf("allocate localhost port for %s/%s:%d: %w", service.Namespace, service.Name, servicePort.Port, err)
	}
	return s.startServiceForward(
		key,
		contextName,
		service.Namespace,
		service.Name,
		localPort,
		int(servicePort.Port),
	)
}

func (s *Server) kubernetesGatewayForward(contextName string) *kubeForward {
	return s.kubernetesForward(contextName + "/gateway")
}

func (s *Server) kubernetesForward(key string) *kubeForward {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kubeForwards[key]
}

func (s *Server) deleteKubernetesClusterRoutes(ctx context.Context, clusterName, contextName string) error {
	contextNames := []string{"porto-" + clusterName}
	if contextName != "" && contextName != contextNames[0] {
		contextNames = append(contextNames, contextName)
	}
	var deleteErrors []error
	for _, contextName := range contextNames {
		if err := s.store.DeleteKubernetesRoutesByContext(ctx, contextName); err != nil {
			deleteErrors = append(deleteErrors, err)
		}
	}
	if len(deleteErrors) == 0 && s.tlsCertificates != nil {
		if _, err := s.ensureProjectCertificate(ctx); err != nil {
			deleteErrors = append(deleteErrors, err)
		}
	}
	return errors.Join(deleteErrors...)
}

func (s *Server) deleteKubernetesHTTPRoutes(ctx context.Context, contextName string) error {
	if contextName == "" {
		return nil
	}
	routes, err := s.store.ListKubernetesRoutes(ctx, contextName)
	if err != nil {
		return err
	}
	var deleteErrors []error
	for _, route := range routes {
		if err := s.kubernetes.DeleteHTTPRoute(
			ctx,
			contextName,
			route.Namespace,
			route.Service,
			route.ServicePort,
		); err != nil {
			deleteErrors = append(deleteErrors, err)
		}
	}
	return errors.Join(deleteErrors...)
}

func kubernetesRouteKey(namespace, service string, port int32) string {
	return namespace + "/" + service + "/" + strconv.Itoa(int(port))
}

func kubernetesRawForwardKey(contextName, namespace, service string, port int32) string {
	return contextName + "/raw/" + kubernetesRouteKey(namespace, service, port)
}

func kubernetesRouteHostname(contextName, namespace, service string, port int32) string {
	cluster := strings.TrimPrefix(contextName, "porto-k3s-")
	cluster = strings.TrimPrefix(cluster, "porto-")
	return kubernetesHostnameLabel(service+"-"+strconv.Itoa(int(port))) + "." +
		kubernetesHostnameLabel(namespace) + "." +
		kubernetesHostnameLabel(cluster)
}

func kubernetesHostnameLabel(value string) string {
	value = strings.Trim(kubernetesHostnameCharacters.ReplaceAllString(strings.ToLower(value), "-"), "-")
	if value == "" {
		return "service"
	}
	if len(value) > 63 {
		value = strings.TrimRight(value[:63], "-")
	}
	return value
}
