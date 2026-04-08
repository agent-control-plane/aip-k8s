package fetchers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ravisantoshgudimetla/aip-k8s/internal/evaluation"
)

// FetchK8sDeployment fetches the state of a Kubernetes Deployment.
// targetURI format: k8s://<cluster>/<namespace>/deployment/<name>
func FetchK8sDeployment(ctx context.Context, c client.Client, targetURI string) (*apiextensionsv1.JSON, error) {
	// Validate the URI has exactly 3 path segments: namespace/resourceType/name.
	// Extra segments (e.g. k8s://prod/apps/deployment/default/name) are silently
	// truncated by ParseTargetURI, which would return the wrong object.
	u, err := url.Parse(targetURI)
	if err != nil || u.Scheme != "k8s" {
		return nil, fmt.Errorf("invalid k8s URI: %s", targetURI)
	}
	pathParts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(pathParts) != 3 {
		return nil, fmt.Errorf("invalid deployment URI %q: expected k8s://<cluster>/<namespace>/deployment/<name>", targetURI)
	}

	parsed := evaluation.ParseTargetURI(targetURI)
	if parsed.ResourceType != "deployment" {
		return nil, fmt.Errorf("invalid resource type for k8s-deployment fetcher: %s", parsed.ResourceType)
	}

	name := parsed.Name
	ns := parsed.Namespace
	if name == "" {
		return nil, fmt.Errorf("invalid deployment URI: %s (missing name)", targetURI)
	}
	if ns == "" {
		return nil, fmt.Errorf("invalid deployment URI: %s (missing namespace)", targetURI)
	}

	result := struct {
		TargetExists        bool   `json:"targetExists"`
		HasActiveEndpoints  bool   `json:"hasActiveEndpoints"`
		ActiveEndpointCount int    `json:"activeEndpointCount"`
		ReadyReplicas       int    `json:"readyReplicas"`
		SpecReplicas        int    `json:"specReplicas"`
		StateFingerprint    string `json:"stateFingerprint,omitempty"`
	}{}

	// 1. Fetch Deployment
	var dep appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, &dep); err == nil {
		result.TargetExists = true
		if dep.Spec.Replicas != nil {
			result.SpecReplicas = int(*dep.Spec.Replicas)
		}
		result.ReadyReplicas = int(dep.Status.ReadyReplicas)
		result.StateFingerprint = dep.ResourceVersion

		// 2. Resolve Services that select this Deployment's pods, then fetch
		// EndpointSlices by Service name. EndpointSlice objects carry the label
		// kubernetes.io/service-name = <Service.Name>, not the Deployment name,
		// so querying by Deployment name would miss differently-named Services.
		if dep.Spec.Selector != nil && len(dep.Spec.Selector.MatchLabels) > 0 {
			var svcList corev1.ServiceList
			if err := c.List(ctx, &svcList,
				client.InNamespace(ns),
				client.MatchingLabels(dep.Spec.Selector.MatchLabels),
			); err == nil {
				for _, svc := range svcList.Items {
					var epList discoveryv1.EndpointSliceList
					if err := c.List(ctx, &epList,
						client.InNamespace(ns),
						client.MatchingLabels{"kubernetes.io/service-name": svc.Name},
					); err == nil {
						for _, eps := range epList.Items {
							for _, ep := range eps.Endpoints {
								if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
									result.ActiveEndpointCount += len(ep.Addresses)
								}
							}
						}
					}
				}
			}
		}
	} else if client.IgnoreNotFound(err) != nil {
		return nil, fmt.Errorf("failed to fetch deployment %s/%s: %w", ns, name, err)
	}
	result.HasActiveEndpoints = result.ActiveEndpointCount > 0

	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}

	return &apiextensionsv1.JSON{Raw: raw}, nil
}
