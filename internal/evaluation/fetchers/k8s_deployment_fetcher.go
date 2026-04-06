package fetchers

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ravisantoshgudimetla/aip-k8s/internal/evaluation"
)

// FetchK8sDeployment fetches the state of a Kubernetes Deployment.
// targetURI format: k8s://<cluster>/<namespace>/deployment/<name>
func FetchK8sDeployment(ctx context.Context, c client.Client, targetURI string) (*apiextensionsv1.JSON, error) {
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
	} else if client.IgnoreNotFound(err) != nil {
		return nil, fmt.Errorf("failed to fetch deployment %s/%s: %w", ns, name, err)
	}

	// 2. Fetch Endpoints
	var epList discoveryv1.EndpointSliceList
	if err := c.List(ctx, &epList,
		client.InNamespace(ns),
		client.MatchingLabels{"kubernetes.io/service-name": name},
	); err == nil {
		for _, eps := range epList.Items {
			// Found endpoints for this service name
			for _, ep := range eps.Endpoints {
				if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
					result.ActiveEndpointCount += len(ep.Addresses)
				}
			}
		}
	} else {
		return nil, fmt.Errorf("failed to list endpoints for %s/%s: %w", ns, name, err)
	}
	result.HasActiveEndpoints = result.ActiveEndpointCount > 0

	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}

	return &apiextensionsv1.JSON{Raw: raw}, nil
}
