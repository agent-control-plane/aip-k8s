package evaluation

import (
	"context"
	"net/url"
	"strings"
)

// TargetContext holds live cluster state for an AgentRequest's target resource.
// It is fetched by the AIP control plane before policy evaluation and injected
// as the `target` variable into CEL expressions.
//
// Agents never provide this data — the control plane owns it. This prevents
// agents from misrepresenting cluster state to bypass governance.
type TargetContext struct {
	// Exists is true when the target resource was found in the cluster.
	Exists bool
	// HasActiveEndpoints is true when the target service has ready endpoint addresses,
	// indicating live traffic is being served.
	HasActiveEndpoints bool
	// ActiveEndpointCount is the number of ready endpoint addresses.
	ActiveEndpointCount int
	// ReadyReplicas is the number of ready replicas for the target deployment.
	ReadyReplicas int
	// SpecReplicas is the desired replica count in the deployment spec.
	SpecReplicas int
	// DownstreamServices lists service names in the same namespace that route
	// traffic to the target deployment's pods.
	DownstreamServices []string
	// StateFingerprint is an opaque token identifying the observed state version.
	// K8s binding: resourceVersion of the target Deployment.
	// Never exposed to agents. Used only for equality comparison by the controller.
	StateFingerprint string
}

// AsMap converts TargetContext to a CEL-compatible map for use as the `target` variable.
func (t *TargetContext) AsMap() map[string]any {
	downstream := make([]any, len(t.DownstreamServices))
	for i, s := range t.DownstreamServices {
		downstream[i] = s
	}
	return map[string]any{
		"exists":              t.Exists,
		"hasActiveEndpoints":  t.HasActiveEndpoints,
		"activeEndpointCount": int64(t.ActiveEndpointCount),
		"readyReplicas":       int64(t.ReadyReplicas),
		"specReplicas":        int64(t.SpecReplicas),
		"downstreamServices":  downstream,
	}
}

// TargetContextFetcher fetches live cluster state for an AgentRequest target URI.
//
// The control plane calls Fetch before CEL evaluation so that policies can
// reference real cluster state without trusting agent-provided data.
//
// Implementations:
//   - KubernetesTargetContextFetcher — production, queries the K8s API
//   - MockTargetContextFetcher       — testing and demos, returns configured data
type TargetContextFetcher interface {
	Fetch(ctx context.Context, targetURI string, namespace string) (*TargetContext, error)
}

// ParsedTargetURI holds the parsed components of a k8s:// target URI.
//
// Format:  k8s://{cluster}/{namespace}/{resourceType}/{name}
// Example: k8s://prod/default/deployment/payment-api
type ParsedTargetURI struct {
	Cluster      string // logical cluster/environment name (e.g. "prod", "staging")
	Namespace    string // Kubernetes namespace (e.g. "default")
	ResourceType string // resource type (e.g. "deployment", "service")
	Name         string // resource name (e.g. "payment-api")
}

// ParseTargetURI parses a k8s:// URI. Returns zero-value struct on failure.
// Callers should check Name != "" for validity.
func ParseTargetURI(uri string) ParsedTargetURI {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "k8s" {
		return ParsedTargetURI{}
	}

	// Host = cluster, Path = /namespace/resourceType/name
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	parsed := ParsedTargetURI{Cluster: u.Host}

	if len(parts) >= 1 {
		parsed.Namespace = parts[0]
	}
	if len(parts) >= 2 {
		parsed.ResourceType = parts[1]
	}
	if len(parts) >= 3 {
		parsed.Name = parts[2]
	}
	return parsed
}
