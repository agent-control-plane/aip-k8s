package credential

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StaticSecretProvider retrieves a static token from a Kubernetes Secret.
type StaticSecretProvider struct {
	client    client.Client
	name      string
	namespace string
	key       string
	cache     *TokenCache
}

// NewStaticSecretProvider creates a new StaticSecretProvider.
func NewStaticSecretProvider(cl client.Client, name, namespace, key string) *StaticSecretProvider {
	p := &StaticSecretProvider{
		client:    cl,
		name:      name,
		namespace: namespace,
		key:       key,
	}
	p.cache = NewTokenCache(p.fetchToken)
	return p
}

func (p *StaticSecretProvider) fetchToken(ctx context.Context) (string, time.Time, error) {
	var secret corev1.Secret
	err := p.client.Get(ctx, types.NamespacedName{Name: p.name, Namespace: p.namespace}, &secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to get secret %s/%s: %w", p.namespace, p.name, err)
	}

	val, ok := secret.Data[p.key]
	if !ok {
		return "", time.Time{}, fmt.Errorf("key %q not found in secret %s/%s", p.key, p.namespace, p.name)
	}

	// Cache static secrets for a reasonable duration (e.g., 5 minutes)
	return string(val), time.Now().Add(5 * time.Minute), nil
}

// Token returns the static secret token.
func (p *StaticSecretProvider) Token(ctx context.Context, rawOIDCToken string) (string, error) {
	return p.cache.Get(ctx)
}
