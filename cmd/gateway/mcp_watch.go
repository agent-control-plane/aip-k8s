package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

const mcpWatchRetryDelay = 2 * time.Second

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
	srv, ok := c.servers[name]
	if !ok {
		return nil
	}
	copy := *srv
	return &copy
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

// listNames returns a snapshot of all server names currently in the cache.
func (c *mcpServerCache) listNames() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.servers))
	for name := range c.servers {
		out = append(out, name)
	}
	return out
}

// upsert atomically replaces the cached entry for name with a new MCPServer
// snapshot. Callers that hold an old pointer continue to see a stable view.
// The upstream session is preserved when URL and BearerToken are unchanged.
func (c *mcpServerCache) upsert(name, url, bearerToken string, tools []MCPTool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var sessionID string
	var sessionReady bool
	if existing, ok := c.servers[name]; ok && existing.URL == url && existing.BearerToken == bearerToken {
		// Preserve the upstream session when endpoint and auth are unchanged.
		sessionID = existing.SessionID
		sessionReady = existing.sessionReady
	}

	c.servers[name] = &MCPServer{
		Name:         name,
		URL:          url,
		BearerToken:  bearerToken,
		Tools:        tools,
		SessionID:    sessionID,
		sessionReady: sessionReady,
		sessionMu:    &sync.Mutex{},
	}
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
			log.Printf("MCPServer watch failed, err=%v, retryDelay=%v", err, mcpWatchRetryDelay)
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

	present := make(map[string]struct{}, len(initialList.Items))
	for i := range initialList.Items {
		present[initialList.Items[i].Name] = struct{}{}
		upsertMCPServerFromCRD(ctx, &initialList.Items[i], cl, cache)
	}
	// Evict stale entries that existed before the relist but are no longer in the API.
	for _, name := range cache.listNames() {
		if _, ok := present[name]; !ok {
			cache.remove(name)
			log.Printf("Removed stale MCPServer from cache, name=%s", name)
		}
	}
	log.Printf("MCPServer watch loaded servers, count=%d", len(initialList.Items))

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
		case watch.Added, watch.Modified:
			if crd, ok := event.Object.(*v1alpha1.MCPServer); ok {
				upsertMCPServerFromCRD(ctx, crd, cl, cache)
			}
		case watch.Deleted:
			if crd, ok := event.Object.(*v1alpha1.MCPServer); ok {
				cache.remove(crd.Name)
				log.Printf("Removed MCPServer from cache, name=%s", crd.Name)
			}
		case watch.Error:
			return fmt.Errorf("watch error: %v", event.Object)
		}
	}
	return nil
}

// upsertMCPServerFromCRD converts an MCPServer CRD to the gateway's MCPServer type
// and inserts it into the cache. Resolves the bearer token from the referenced Secret.
// On transient Secret fetch errors the existing cached token is preserved and the
// upsert is skipped so healthy entries are not overwritten.
func upsertMCPServerFromCRD(ctx context.Context, crd *v1alpha1.MCPServer, cl client.Client, cache *mcpServerCache) {
	name := crd.Name
	url := crd.Spec.URL

	var bearerToken string
	if crd.Spec.BearerTokenSecretRef != nil {
		secretNS := crd.Spec.SecretNamespace
		// MCPServer is cluster-scoped; crd.Namespace is always "".
		// CEL validation enforces secretNamespace is non-empty when
		// bearerTokenSecretRef is set, so an empty value here means
		// a misconfigured CRD that slipped past validation.
		if secretNS == "" {
			log.Printf("Skipped Secret lookup, mcpServer=%s, reason=secretNamespace is empty for cluster-scoped MCPServer", name)
		} else {
			ref := crd.Spec.BearerTokenSecretRef
			key := ref.Key
			if key == "" {
				key = "token"
			}
			var secret corev1.Secret
			nn := types.NamespacedName{Name: ref.Name, Namespace: secretNS}
			if err := cl.Get(ctx, nn, &secret); err != nil {
				if !apierrors.IsNotFound(err) {
					// Transient error (network, timeout, etc.) — keep the existing token and
					// skip updating this entry so a healthy cached server is not corrupted.
					log.Printf("Failed to get Secret, secret=%s, mcpServer=%s, err=%v", nn.String(), name, err)
					return
				}
				// Secret was deleted — proceed with empty token (auth removed).
			} else if tokenBytes, ok := secret.Data[key]; ok {
				bearerToken = string(tokenBytes)
			} else {
				// Secret exists but key is missing — proceed with empty token.
				log.Printf("Secret missing expected key, secret=%s, key=%s, mcpServer=%s", nn.String(), key, name)
			}
		}
	}

	// Status.Tools is the source of truth for discovered tools (populated by the controller).
	tools := make([]MCPTool, 0, len(crd.Status.Tools))
	for _, t := range crd.Status.Tools {
		tools = append(tools, MCPTool{
			Name:     t.Name,
			ReadOnly: t.ReadOnly,
		})
	}

	// On transient Secret fetch errors we returned early above, preserving the old entry.
	// When the Secret is not found or the key is missing we intentionally clear the token.
	cache.upsert(name, url, bearerToken, tools)
	log.Printf("Upserted MCPServer, name=%s, url=%s, tools=%d", name, url, len(tools))
}
