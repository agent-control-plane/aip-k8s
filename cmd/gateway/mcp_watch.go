package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

const (
	mcpWatchReListInterval = 5 * time.Minute
	mcpWatchRetryDelay     = 2 * time.Second
)

// mcpServerCache is a thread-safe in-memory cache of MCPServer CRDs.
type mcpServerCache struct {
	mu      sync.RWMutex
	servers map[string]*MCPServer // keyed by CRD name
}

func newMCPServerCache() *mcpServerCache {
	return &mcpServerCache{
		servers: make(map[string]*MCPServer),
	}
}

func (c *mcpServerCache) get(name string) *MCPServer {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.servers[name]
}

func (c *mcpServerCache) getAll() []MCPServer {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]MCPServer, 0, len(c.servers))
	for _, s := range c.servers {
		out = append(out, *s)
	}
	return out
}

func (c *mcpServerCache) upsert(name, url, bearerToken string, tools []MCPTool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	existing, ok := c.servers[name]
	if !ok {
		existing = &MCPServer{
			Name:         name,
			URL:          url,
			sessionMu:    &sync.Mutex{},
			sessionReady: false,
		}
		c.servers[name] = existing
	}
	existing.URL = url
	existing.BearerToken = bearerToken
	existing.Tools = tools
	existing.SessionID = ""
	existing.sessionReady = false
}

func (c *mcpServerCache) remove(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.servers, name)
}

// watchMCPServers runs a background loop that lists and watches MCPServer CRDs,
// populating the in-memory cache. Blocks until ctx is cancelled.
func watchMCPServers(ctx context.Context, cl client.WithWatch, cache *mcpServerCache) {
	for {
		if err := watchMCPServersOnce(ctx, cl, cache); err != nil {
			log.Printf("MCPServer watch error: %v; retrying in %v", err, mcpWatchRetryDelay)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(mcpWatchRetryDelay):
		}
	}
}

// watchMCPServersOnce performs a single list+watch cycle.
func watchMCPServersOnce(ctx context.Context, cl client.WithWatch, cache *mcpServerCache) error {
	var initialList v1alpha1.MCPServerList
	listErr := retry.OnError(retry.DefaultRetry, func(err error) bool {
		return err != nil
	}, func() error {
		return cl.List(ctx, &initialList)
	})
	if listErr != nil {
		return listErr
	}

	for i := range initialList.Items {
		upsertMCPServerFromCRD(&initialList.Items[i], cl, cache)
	}
	log.Printf("MCPServer watch: loaded %d servers", len(initialList.Items))

	rv := initialList.ResourceVersion
	watcher, err := cl.Watch(ctx, &v1alpha1.MCPServerList{}, &client.ListOptions{
		FieldSelector: fields.Everything(),
		Raw:           &metav1.ListOptions{ResourceVersion: rv},
	})
	if err != nil {
		return err
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		switch event.Type {
		case "ADDED", "MODIFIED":
			if crd, ok := event.Object.(*v1alpha1.MCPServer); ok {
				upsertMCPServerFromCRD(crd, cl, cache)
			}
		case "DELETED":
			if crd, ok := event.Object.(*v1alpha1.MCPServer); ok {
				cache.remove(crd.Name)
				log.Printf("MCPServer watch: removed %s", crd.Name)
			}
		case "ERROR":
			return fmt.Errorf("watch error: %v", event.Object)
		}
	}
	return nil
}

// upsertMCPServerFromCRD converts an MCPServer CRD to the gateway's MCPServer type
// and inserts it into the cache. Resolves the bearer token from the referenced Secret.
func upsertMCPServerFromCRD(crd *v1alpha1.MCPServer, cl client.Client, cache *mcpServerCache) {
	name := crd.Name
	url := crd.Spec.URL

	var bearerToken string
	if crd.Spec.BearerTokenSecretRef != nil {
		secretNS := crd.Spec.SecretNamespace
		if secretNS == "" {
			secretNS = crd.Namespace
		}
		ref := crd.Spec.BearerTokenSecretRef
		key := ref.Key
		if key == "" {
			key = "token"
		}
		var secret corev1.Secret
		nn := types.NamespacedName{Name: ref.Name, Namespace: secretNS}
		if err := cl.Get(context.Background(), nn, &secret); err != nil {
			log.Printf("MCPServer watch: failed to get secret %s/%s for %s: %v", secretNS, ref.Name, name, err)
		} else if tokenBytes, ok := secret.Data[key]; ok {
			bearerToken = string(tokenBytes)
		} else {
			log.Printf("MCPServer watch: secret %s/%s has no key %q for %s", secretNS, ref.Name, key, name)
		}
	}

	readOnlySet := make(map[string]bool, len(crd.Spec.ReadOnlyTools))
	for _, t := range crd.Spec.ReadOnlyTools {
		readOnlySet[t] = true
	}

	tools := make([]MCPTool, 0, len(crd.Status.Tools))
	for _, t := range crd.Status.Tools {
		tools = append(tools, MCPTool{
			Name:     t.Name,
			ReadOnly: t.ReadOnly,
		})
	}

	cache.upsert(name, url, bearerToken, tools)
	log.Printf("MCPServer watch: upserted %s (%s, %d tools)", name, url, len(tools))
}
