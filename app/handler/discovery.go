package handler

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BackendSource defines where backend URLs are loaded from.
// The implementation can later be backed by a Kubernetes watch, but the interface stays simple
// so the gateway can refresh the route table at runtime without coupling to k8s packages.
type BackendSource interface {
	Discover() ([]string, error)
}

// ModelBackendSource returns backends partitioned by model name.
// This is the real source of truth for model-aware routing.
type ModelBackendSource interface {
	DiscoverByModel() (map[string][]string, error)
}

// StaticBackendSource is the default implementation used in local/dev and test modes.
type StaticBackendSource struct {
	urls []string
}

// KubernetesBackendSource reads ready Endpoints from the Kubernetes API and converts them into backend URLs.
type KubernetesBackendSource struct {
	namespace string
	port      int32
	path      string
	baseURL   string
	tokenPath string
	caFile    string
	httpClient *http.Client
}

type k8sMetadata struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

type k8sEndpointList struct {
	Items []k8sEndpointItem `json:"items"`
}

type k8sEndpointItem struct {
	Metadata k8sMetadata        `json:"metadata"`
	Subsets  []k8sEndpointSubset `json:"subsets"`
}

type k8sServiceList struct {
	Items []k8sServiceItem `json:"items"`
}

type k8sServiceItem struct {
	Metadata k8sMetadata `json:"metadata"`
}

type k8sEndpointSubset struct {
	Addresses []k8sEndpointAddress `json:"addresses"`
	Ports     []k8sEndpointPort    `json:"ports"`
}

type k8sEndpointAddress struct {
	IP string `json:"ip"`
}

type k8sEndpointPort struct {
	Port int32 `json:"port"`
}

func NewStaticBackendSource(urls []string) *StaticBackendSource {
	return &StaticBackendSource{urls: normalizeURLs(urls)}
}

func NewStaticBackendSourceFromEnv(envKey string, fallback []string) *StaticBackendSource {
	urls := append([]string{}, fallback...)
	if envKey != "" {
		value := strings.TrimSpace(os.Getenv(envKey))
		if value != "" {
			urls = strings.Split(value, ",")
		}
	}
	return NewStaticBackendSource(urls)
}

func NewKubernetesBackendSource(namespace string, baseURL string, port int32) *KubernetesBackendSource {
	if namespace == "" {
		namespace = "default"
	}
	if baseURL == "" {
		baseURL = "https://kubernetes.default.svc"
	}
	if port == 0 {
		port = 8000
	}

	caFile := "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	if _, err := os.Stat(caFile); err != nil {
		caFile = ""
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}

	if caFile != "" {
		client.Transport.(*http.Transport).TLSClientConfig.RootCAs = mustRootCAs(caFile)
	}

	return &KubernetesBackendSource{
		namespace: namespace,
		port:      port,
		path:      "/v1/chat/completions",
		baseURL:   strings.TrimRight(baseURL, "/"),
		tokenPath: "/var/run/secrets/kubernetes.io/serviceaccount/token",
		caFile:    caFile,
		httpClient: client,
	}
}

func NewKubernetesBackendSourceFromEnv() (*KubernetesBackendSource, error) {
	namespace := strings.TrimSpace(os.Getenv("KUBE_NAMESPACE"))
	if namespace == "" {
		namespace = "default"
	}

	baseURL := strings.TrimSpace(os.Getenv("KUBE_API_SERVER"))
	if baseURL == "" {
		baseURL = "https://kubernetes.default.svc"
	}

	port := int32(8000)
	if value := strings.TrimSpace(os.Getenv("BACKEND_PORT")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err == nil && parsed > 0 {
			port = int32(parsed)
		}
	}

	source := NewKubernetesBackendSource(namespace, baseURL, port)
	if _, err := os.Stat(source.tokenPath); err != nil {
		return nil, err
	}
	return source, nil
}

func mustRootCAs(caFile string) *x509.CertPool {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pem, err := os.ReadFile(caFile)
	if err == nil {
		pool.AppendCertsFromPEM(pem)
	}
	return pool
}

func (s *StaticBackendSource) Discover() ([]string, error) {
	if s == nil {
		return nil, nil
	}
	return append([]string{}, normalizeURLs(s.urls)...), nil
}

func (s *StaticBackendSource) DiscoverByModel() (map[string][]string, error) {
	if s == nil {
		return nil, nil
	}
	return map[string][]string{"default": normalizeURLs(s.urls)}, nil
}

func (s *KubernetesBackendSource) DiscoverByModel() (map[string][]string, error) {
	if s == nil || s.httpClient == nil {
		return nil, fmt.Errorf("kubernetes backend source is not configured")
	}

	token, err := os.ReadFile(s.tokenPath)
	if err != nil {
		return nil, err
	}

	servicesURL := fmt.Sprintf("%s/api/v1/namespaces/%s/services", s.baseURL, s.namespace)
	servicesPayload, err := s.callKubernetes(token, servicesURL)
	if err != nil {
		return nil, err
	}
	var services k8sServiceList
	if err := json.Unmarshal(servicesPayload, &services); err != nil {
		return nil, err
	}

	endpointsURL := fmt.Sprintf("%s/api/v1/namespaces/%s/endpoints", s.baseURL, s.namespace)
	endpointsPayload, err := s.callKubernetes(token, endpointsURL)
	if err != nil {
		return nil, err
	}
	var endpoints k8sEndpointList
	if err := json.Unmarshal(endpointsPayload, &endpoints); err != nil {
		return nil, err
	}

	byName := make(map[string][]string)
	for _, item := range endpoints.Items {
		name := strings.TrimSpace(item.Metadata.Name)
		if name == "" {
			continue
		}
		urls := extractURLList(s, item.Subsets, s.port)
		if len(urls) == 0 {
			continue
		}
		byName[name] = normalizeURLs(urls)
	}

	result := make(map[string][]string)
	for _, service := range services.Items {
		modelName := detectModelName(service.Metadata.Name, service.Metadata.Labels)
		if modelName == "" {
			continue
		}
		urls, ok := byName[service.Metadata.Name]
		if !ok || len(urls) == 0 {
			continue
		}
		result[modelName] = normalizeURLs(urls)
	}

	return result, nil
}

func (s *KubernetesBackendSource) Discover() ([]string, error) {
	if s == nil || s.httpClient == nil {
		return nil, fmt.Errorf("kubernetes backend source is not configured")
	}

	token, err := os.ReadFile(s.tokenPath)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/api/v1/namespaces/%s/endpoints", s.baseURL, s.namespace)
	body, err := s.callKubernetes(token, url)
	if err != nil {
		return nil, err
	}

	var list k8sEndpointList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}

	urls := make([]string, 0)
	seen := make(map[string]struct{})
	for _, item := range list.Items {
		for _, subset := range item.Subsets {
			for _, addr := range subset.Addresses {
				if strings.TrimSpace(addr.IP) == "" {
					continue
				}
				url := s.backendURL(addr.IP, detectPort(subset, s.port))
				if _, exists := seen[url]; exists {
					continue
				}
				seen[url] = struct{}{}
				urls = append(urls, url)
			}
		}
	}

	sort.Strings(urls)
	return normalizeURLs(urls), nil
}

func (s *KubernetesBackendSource) callKubernetes(token []byte, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("kubernetes api returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return io.ReadAll(resp.Body)
}

func detectModelName(name string, labels map[string]string) string {
	for _, key := range []string{
		"llm-model",
		"llm_model",
		"model",
		"model-name",
		"model_name",
		"inference-model",
		"serving-model",
		"serving.trin.io/model",
		"app.kubernetes.io/name",
		"app.kubernetes.io/instance",
	} {
		if value := strings.TrimSpace(labels[key]); value != "" {
			return normalizeModelKey(value)
		}
	}
	return normalizeModelKey(strings.TrimSpace(name))
}

func detectPort(subset k8sEndpointSubset, defaultPort int32) int32 {
	if len(subset.Ports) > 0 && subset.Ports[0].Port != 0 {
		return subset.Ports[0].Port
	}
	return defaultPort
}

func extractURLList(source *KubernetesBackendSource, subsets []k8sEndpointSubset, defaultPort int32) []string {
	urls := make([]string, 0)
	seen := make(map[string]struct{})
	for _, subset := range subsets {
		port := detectPort(subset, defaultPort)
		for _, addr := range subset.Addresses {
			if strings.TrimSpace(addr.IP) == "" {
				continue
			}
			url := source.backendURL(addr.IP, port)
			if _, exists := seen[url]; exists {
				continue
			}
			seen[url] = struct{}{}
			urls = append(urls, url)
		}
	}
	return urls
}

func (s *KubernetesBackendSource) backendURL(ip string, port int32) string {
	path := s.path
	if path == "" {
		path = "/v1/chat/completions"
	}
	return fmt.Sprintf("http://%s:%d%s", ip, port, path)
}

func normalizeURLs(urls []string) []string {
	seen := make(map[string]struct{}, len(urls))
	result := make([]string, 0, len(urls))
	for _, raw := range urls {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		value = strings.TrimRight(value, "/")
		if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func normalizeModelKey(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}

	var builder strings.Builder
	builder.Grow(len(trimmed))
	lastSeparator := false
	for _, r := range strings.ToLower(trimmed) {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
			lastSeparator = false
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastSeparator = false
		case r == '-', r == '_', r == '.', r == '/', r == ':', r == ' ', r == '\\':
			if !lastSeparator {
				builder.WriteRune('-')
				lastSeparator = true
			}
		default:
			if !lastSeparator {
				builder.WriteRune('-')
				lastSeparator = true
			}
		}
	}
	result := strings.Trim(builder.String(), "-_.")
	if result == "" {
		return "default"
	}
	return result
}

// StartBackendRefresh runs a periodic refresh loop that keeps the router synchronized with the backend source.
func StartBackendRefresh(ctx context.Context, source BackendSource, router Router, interval time.Duration) {
	if source == nil || router == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}

	go func() {
		for {
			refreshBackends(source, router)

			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()
}

func refreshBackends(source BackendSource, router Router) {
	if modelSource, ok := source.(ModelBackendSource); ok {
		if modelRouter, ok := router.(*ModelConsistentHashRouter); ok {
			if modelBackends, err := modelSource.DiscoverByModel(); err == nil {
				modelRouter.UpdateModelBackends(modelBackends)
			}
			return
		}
	}

	if urls, err := source.Discover(); err == nil {
		router.UpdateBackends(urls)
	}
}
