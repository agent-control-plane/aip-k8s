package evaluation

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubernetesTargetContextFetcher fetches live cluster state via the Kubernetes API.
// Used in production by the AIP controller.
type KubernetesTargetContextFetcher struct {
	Client client.Reader
}

// Fetch queries live cluster state for the given target URI.
// Never returns an error for missing resources — missing = zero-value context.
// Only returns errors for genuine API server failures.
func (f *KubernetesTargetContextFetcher) Fetch(ctx context.Context, targetURI string, namespace string) (*TargetContext, error) {
	parsed := ParseTargetURI(targetURI)
	if parsed.Name == "" {
		return &TargetContext{}, nil
	}

	// The URI namespace (e.g. "default") is the K8s namespace.
	// Fall back to the AgentRequest's own namespace if URI namespace is missing.
	ns := parsed.Namespace
	if ns == "" {
		ns = namespace
	}

	result := &TargetContext{}

	if parsed.ResourceType == "deployment" {
		f.fetchDeploymentState(ctx, parsed.Name, ns, result)
	}

	// Check endpoints regardless of resource type — any resource that has
	// a matching Service will show active endpoints when traffic is live.
	f.fetchEndpointState(ctx, parsed.Name, ns, result)

	return result, nil
}

func (f *KubernetesTargetContextFetcher) fetchDeploymentState(ctx context.Context, name, namespace string, result *TargetContext) {
	var dep appsv1.Deployment
	if err := f.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &dep); err != nil {
		// Transient error — leave replica counts at zero, don't fail evaluation
		return
	}
	result.Exists = true
	if dep.Spec.Replicas != nil {
		result.SpecReplicas = int(*dep.Spec.Replicas)
	}
	result.ReadyReplicas = int(dep.Status.ReadyReplicas)
	result.StateFingerprint = dep.ResourceVersion
}

func (f *KubernetesTargetContextFetcher) fetchEndpointState(ctx context.Context, name, namespace string, result *TargetContext) {
	var epList discoveryv1.EndpointSliceList
	if err := f.Client.List(ctx, &epList,
		client.InNamespace(namespace),
		client.MatchingLabels{"kubernetes.io/service-name": name},
	); err != nil {
		return
	}
	for _, eps := range epList.Items {
		result.Exists = true
		for _, ep := range eps.Endpoints {
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				result.ActiveEndpointCount += len(ep.Addresses)
			}
		}
	}
	result.HasActiveEndpoints = result.ActiveEndpointCount > 0
}
