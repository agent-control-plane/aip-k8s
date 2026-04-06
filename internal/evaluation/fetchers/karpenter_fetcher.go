package fetchers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FetchKarpenter fetches the state of a Karpenter NodePool.
// targetURI format: k8s://<cluster>/karpenter.sh/nodepool/<name>
func FetchKarpenter(ctx context.Context, c client.Client, targetURI string) (*apiextensionsv1.JSON, error) {
	u, err := url.Parse(targetURI)
	if err != nil || u.Scheme != "k8s" {
		return nil, fmt.Errorf("invalid Karpenter URI: %s", targetURI)
	}

	// /karpenter.sh/nodepool/<name>
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "karpenter.sh" || parts[1] != "nodepool" {
		return nil, fmt.Errorf("invalid Karpenter NodePool URI: %s", targetURI)
	}
	name := parts[2]

	result := struct {
		CurrentLimitCPU           string         `json:"currentLimitCPU"`
		CurrentLimitMemory        string         `json:"currentLimitMemory"`
		CurrentNodeCount          int            `json:"currentNodeCount"`
		PendingPods               int            `json:"pendingPods"`
		EstimatedCostDeltaPerHour string         `json:"estimatedCostDeltaPerHour"`
		RecentScalingEvents       []ScalingEvent `json:"recentScalingEvents"`
	}{
		EstimatedCostDeltaPerHour: "$0.00",
		RecentScalingEvents:       []ScalingEvent{},
	}

	// 1. Fetch NodePool
	nodepool := &unstructured.Unstructured{}
	nodepool.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "karpenter.sh",
		Version: "v1beta1",
		Kind:    "NodePool",
	})
	if err := c.Get(ctx, client.ObjectKey{Name: name}, nodepool); err == nil {
		limits, found, _ := unstructured.NestedMap(nodepool.Object, "spec", "limits")
		if found {
			if cpu, ok := limits["cpu"].(string); ok {
				result.CurrentLimitCPU = cpu
			}
			if mem, ok := limits["memory"].(string); ok {
				result.CurrentLimitMemory = mem
			}
		}
	} else if client.IgnoreNotFound(err) != nil {
		return nil, fmt.Errorf("failed to fetch NodePool %s: %w", name, err)
	}

	// 2. Count Nodes
	var nodeList corev1.NodeList
	if err := c.List(ctx, &nodeList, client.MatchingLabels{"karpenter.sh/nodepool": name}); err == nil {
		result.CurrentNodeCount = len(nodeList.Items)
	} else {
		return nil, fmt.Errorf("failed to list nodes for NodePool %s: %w", name, err)
	}

	// 3. Count Pending Pods
	var podList corev1.PodList
	if err := c.List(ctx, &podList); err == nil {
		count := 0
		for _, pod := range podList.Items {
			if pod.Status.Phase == corev1.PodPending {
				count++
			}
		}
		result.PendingPods = count
	} else {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}

	return &apiextensionsv1.JSON{Raw: raw}, nil
}

type ScalingEvent struct {
	Time      string `json:"time"`
	Direction string `json:"direction"`
	Delta     int    `json:"delta"`
}
