package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/credential"
)

const regWatchRetryDelay = 2 * time.Second

// registrationCache is a thread-safe in-memory cache of AgentRegistration CRDs.
type registrationCache struct {
	k8sClient client.Client
	mu        sync.RWMutex
	byAgent   map[string]*v1alpha1.AgentRegistration
	providers map[string]map[string]credential.Provider // agentIdentity -> service -> Provider
}

func newRegistrationCache(k8sClient client.Client) *registrationCache {
	return &registrationCache{
		k8sClient: k8sClient,
		byAgent:   make(map[string]*v1alpha1.AgentRegistration),
		providers: make(map[string]map[string]credential.Provider),
	}
}

// getForSubject looks up a registration by claimed agent identity and/or caller subject.
//
// Identity resolution rules (secure-by-default):
//   - Step 1: if agentIdentity is given and a registration exists for it:
//   - AllowedSubjects non-empty → require sub ∈ AllowedSubjects.
//   - AllowedSubjects empty    → require sub == agentIdentity OR sub == "" (unauthenticated).
//     An empty AllowedSubjects does NOT mean "accept any caller" — it means the
//     caller must prove they are exactly that agent. This closes the #37 impersonation vector.
//   - Step 2: scan all registrations for one whose AllowedSubjects contains sub.
//     If multiple registrations match, the lookup is considered ambiguous and nil is returned.
//   - Otherwise nil.
func (c *registrationCache) getForSubject(agentIdentity, sub string) *v1alpha1.AgentRegistration {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// 1. If agentIdentity is specified, try a direct lookup first.
	if agentIdentity != "" {
		if reg, ok := c.byAgent[agentIdentity]; ok {
			if reg.Spec.OIDC != nil && len(reg.Spec.OIDC.AllowedSubjects) > 0 {
				// AllowedSubjects configured: sub must be in the list.
				if slices.Contains(reg.Spec.OIDC.AllowedSubjects, sub) {
					return reg
				}
				return nil // sub not in AllowedSubjects — reject immediately
			}
			// No AllowedSubjects: only match if sub equals the agent identity or
			// authentication is not required (sub is empty).
			if sub == "" || sub == agentIdentity {
				return reg
			}
			return nil // sub doesn't match agentIdentity — reject immediately
		}
	}

	// 2. Scan all registrations for one whose AllowedSubjects includes this sub.
	// Reject the lookup if multiple registrations match (ambiguous claim).
	if sub == "" {
		return nil
	}
	var match *v1alpha1.AgentRegistration
	for _, reg := range c.byAgent {
		if reg.Spec.OIDC != nil && slices.Contains(reg.Spec.OIDC.AllowedSubjects, sub) {
			if match != nil {
				// Ambiguous: sub is in multiple registrations' AllowedSubjects.
				log.Printf("Ambiguous AgentRegistration lookup: sub %q matches multiple registrations, rejecting", sub)
				return nil
			}
			match = reg
		}
	}
	return match
}

// get returns the Registration for agentIdentity, or nil if not found.
func (c *registrationCache) get(agentIdentity string) *v1alpha1.AgentRegistration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byAgent[agentIdentity]
}

// exists reports whether a registration exists for the given agent identity,
// regardless of AllowedSubjects. Use this to distinguish "agent not registered"
// from "agent registered but subject claim did not match".
func (c *registrationCache) exists(agentIdentity string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.byAgent[agentIdentity]
	return ok
}

// providerFor returns the CredentialProvider for (agentIdentity, service).
func (c *registrationCache) providerFor(agentIdentity, service string) credential.Provider {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if svcProviders, ok := c.providers[agentIdentity]; ok {
		return svcProviders[service]
	}
	return nil
}

// listAgents returns a snapshot of all agent identities currently in the cache.
func (c *registrationCache) listAgents() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.byAgent))
	for agentID := range c.byAgent {
		out = append(out, agentID)
	}
	return out
}

// upsert atomically replaces the cached entry and instantiates its credential providers.
func (c *registrationCache) upsert(reg *v1alpha1.AgentRegistration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	agentID := reg.Spec.AgentIdentity
	if agentID == "" {
		return
	}

	c.byAgent[agentID] = reg

	// Build providers for this agent
	svcProviders := make(map[string]credential.Provider)
	for _, binding := range reg.Spec.ExternalIdentities {
		var provider credential.Provider
		switch binding.Type {
		case v1alpha1.ExternalIdentityStaticSecret:
			if binding.StaticSecret != nil {
				provider = credential.NewStaticSecretProvider(
					c.k8sClient,
					binding.StaticSecret.Name,
					binding.StaticSecret.Namespace,
					binding.StaticSecret.Key,
				)
			}
		case v1alpha1.ExternalIdentityAzureWorkloadIdentity:
			if binding.AzureWorkloadIdentity != nil {
				provider = credential.NewAzureWorkloadIdentityProvider(
					binding.AzureWorkloadIdentity.TenantID,
					binding.AzureWorkloadIdentity.ClientID,
					binding.AzureWorkloadIdentity.Scope,
				)
			}
		case v1alpha1.ExternalIdentityAWSWebIdentity:
			provider = credential.NewAWSWebIdentityProvider()
		case v1alpha1.ExternalIdentityKubernetesOIDC:
			if binding.KubernetesOIDC != nil {
				provider = credential.NewKubernetesOIDCProvider(
					binding.KubernetesOIDC.TokenExchangeURL,
					binding.KubernetesOIDC.Audience,
				)
			}
		case v1alpha1.ExternalIdentityKubernetesTokenRequest:
			if binding.KubernetesTokenRequest != nil {
				provider = credential.NewKubernetesTokenRequestProvider(
					c.k8sClient,
					binding.KubernetesTokenRequest.ServiceAccountName,
					binding.KubernetesTokenRequest.ServiceAccountNamespace,
					binding.KubernetesTokenRequest.ExpirationSeconds,
					binding.KubernetesTokenRequest.Audiences,
				)
			}
		}

		if provider != nil {
			svcProviders[binding.Service] = provider
		}
	}

	c.providers[agentID] = svcProviders
}

// remove deletes the registration from the cache.
func (c *registrationCache) remove(agentIdentity string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byAgent, agentIdentity)
	delete(c.providers, agentIdentity)
}

// watchAgentRegistrations runs a background loop that lists and watches AgentRegistration CRDs.
func watchAgentRegistrations(ctx context.Context, cl client.WithWatch, cache *registrationCache) {
	for {
		if err := watchAgentRegistrationsOnce(ctx, cl, cache); err != nil {
			log.Printf("AgentRegistration watch failed, err=%v, retryDelay=%v", err, regWatchRetryDelay)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(regWatchRetryDelay):
		}
	}
}

// watchAgentRegistrationsOnce performs a single list+watch cycle.
func watchAgentRegistrationsOnce(ctx context.Context, cl client.WithWatch, cache *registrationCache) error {
	var initialList v1alpha1.AgentRegistrationList
	listErr := retry.OnError(retry.DefaultRetry, func(err error) bool {
		return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	}, func() error {
		return cl.List(ctx, &initialList)
	})
	if listErr != nil {
		return listErr
	}

	present := make(map[string]struct{}, len(initialList.Items))
	for i := range initialList.Items {
		reg := &initialList.Items[i]
		if reg.Spec.AgentIdentity != "" {
			present[reg.Spec.AgentIdentity] = struct{}{}
			cache.upsert(reg)
		}
	}

	// Evict stale entries
	for _, agentID := range cache.listAgents() {
		if _, ok := present[agentID]; !ok {
			cache.remove(agentID)
			log.Printf("Removed stale AgentRegistration from cache, agentIdentity=%s", agentID)
		}
	}
	log.Printf("AgentRegistration watch loaded registrations, count=%d", len(initialList.Items))

	rv := initialList.ResourceVersion
	watcher, err := cl.Watch(ctx, &v1alpha1.AgentRegistrationList{}, &client.ListOptions{
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
			if reg, ok := event.Object.(*v1alpha1.AgentRegistration); ok {
				if reg.Spec.AgentIdentity != "" {
					cache.upsert(reg)
					log.Printf("Upserted AgentRegistration, agentIdentity=%s", reg.Spec.AgentIdentity)
				}
			}
		case watch.Deleted:
			if reg, ok := event.Object.(*v1alpha1.AgentRegistration); ok {
				if reg.Spec.AgentIdentity != "" {
					cache.remove(reg.Spec.AgentIdentity)
					log.Printf("Removed AgentRegistration from cache, agentIdentity=%s", reg.Spec.AgentIdentity)
				}
			}
		case watch.Error:
			return fmt.Errorf("watch error: %v", event.Object)
		}
	}
	return nil
}
