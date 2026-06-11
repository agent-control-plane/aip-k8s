package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTokenCacheSingleflight(t *testing.T) {
	var callCount int32
	fetch := func(ctx context.Context) (string, time.Time, error) {
		atomic.AddInt32(&callCount, 1)
		time.Sleep(100 * time.Millisecond) // Ensure overlap
		return "token-val", time.Now().Add(1 * time.Hour), nil
	}

	cache := NewTokenCache(fetch)

	var wg sync.WaitGroup
	const concurrentCount = 10
	results := make([]string, concurrentCount)
	errors := make([]error, concurrentCount)

	for i := range concurrentCount {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			token, err := cache.Get(context.Background())
			results[idx] = token
			errors[idx] = err
		}(i)
	}

	wg.Wait()

	if atomic.LoadInt32(&callCount) != 1 {
		t.Errorf("expected fetch to be called exactly once, but was called %d times", callCount)
	}

	for i := range concurrentCount {
		if errors[i] != nil {
			t.Errorf("goroutine %d failed: %v", i, errors[i])
		}
		if results[i] != "token-val" {
			t.Errorf("goroutine %d got unexpected result: %s", i, results[i])
		}
	}
}

func TestStaticSecretProvider(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add corev1 scheme: %v", err)
	}

	secretName := "my-secret"
	namespace := "default"
	key := "token"
	tokenVal := "secret-token-123"

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			key: []byte(tokenVal),
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	provider := NewStaticSecretProvider(fakeClient, secretName, namespace, key)

	// Fetch token
	token, err := provider.Token(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != tokenVal {
		t.Errorf("expected %q, got %q", tokenVal, token)
	}

	// Modify secret and verify cache returns the old value
	secret.Data[key] = []byte("new-token-456")
	if err := fakeClient.Update(context.Background(), secret); err != nil {
		t.Fatalf("failed to update secret: %v", err)
	}

	token2, err := provider.Token(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token2 != tokenVal {
		t.Errorf("expected cache to return cached token %q, but got %q", tokenVal, token2)
	}

	// Re-construct provider to bypass cache (simulates update/invalidation in watch)
	provider2 := NewStaticSecretProvider(fakeClient, secretName, namespace, key)
	token3, err := provider2.Token(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token3 != "new-token-456" {
		t.Errorf("expected new token %q, got %q", "new-token-456", token3)
	}
}

func TestAzureWorkloadIdentityProvider(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)

		if r.Method != "POST" {
			t.Errorf("expected POST method, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("expected application/x-www-form-urlencoded content type, got %s", r.Header.Get("Content-Type"))
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read body: %v", err)
		}
		values, err := url.ParseQuery(string(bodyBytes))
		if err != nil {
			t.Fatalf("failed to parse query: %v", err)
		}

		if values.Get("grant_type") != "client_credentials" {
			t.Errorf("expected grant_type=client_credentials, got %s", values.Get("grant_type"))
		}
		if values.Get("client_id") != "my-client-id" {
			t.Errorf("expected client_id=my-client-id, got %s", values.Get("client_id"))
		}
		if values.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
			t.Errorf("expected client_assertion_type jwt-bearer, got %s", values.Get("client_assertion_type"))
		}
		if values.Get("client_assertion") != "oidc-jwt-token" {
			t.Errorf("expected client_assertion=oidc-jwt-token, got %s", values.Get("client_assertion"))
		}
		if values.Get("scope") != "my-scope" {
			t.Errorf("expected scope=my-scope, got %s", values.Get("scope"))
		}

		resp := azureTokenResponse{
			AccessToken: "azure-access-token",
			TokenType:   "Bearer",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewAzureWorkloadIdentityProvider("my-tenant", "my-client-id", "my-scope")
	provider.WithTokenURL(server.URL)

	// Fetch token
	token, err := provider.Token(context.Background(), "oidc-jwt-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "azure-access-token" {
		t.Errorf("expected azure-access-token, got %s", token)
	}

	// Fetch token again to verify caching
	token2, err := provider.Token(context.Background(), "oidc-jwt-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token2 != "azure-access-token" {
		t.Errorf("expected cached azure-access-token, got %s", token2)
	}

	if atomic.LoadInt32(&callCount) != 1 {
		t.Errorf("expected exactly 1 exchange call, got %d", callCount)
	}
}

func TestKubernetesOIDCProvider(t *testing.T) {
	t.Run("PassthroughMode_IsRejected", func(t *testing.T) {
		// Passthrough mode (empty tokenExchangeURL) is intentionally unsupported:
		// forwarding a gateway-audience token to upstream MCP servers allows a
		// compromised server to replay it against the gateway.
		provider := NewKubernetesOIDCProvider("", "")
		_, err := provider.Token(context.Background(), "raw-token-123")
		if err == nil {
			t.Fatal("expected error for passthrough mode, got nil")
		}
	})

	t.Run("ExchangeMode", func(t *testing.T) {
		var callCount int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&callCount, 1)

			if r.Method != "POST" {
				t.Errorf("expected POST method, got %s", r.Method)
			}
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("failed to read body: %v", err)
			}
			values, err := url.ParseQuery(string(bodyBytes))
			if err != nil {
				t.Fatalf("failed to parse query: %v", err)
			}

			if values.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" {
				t.Errorf("expected grant_type=token-exchange, got %s", values.Get("grant_type"))
			}
			if values.Get("subject_token") != "inbound-oidc-jwt" {
				t.Errorf("expected subject_token=inbound-oidc-jwt, got %s", values.Get("subject_token"))
			}
			if values.Get("subject_token_type") != "urn:ietf:params:oauth:token-type:access_token" {
				t.Errorf("expected subject_token_type access_token, got %s", values.Get("subject_token_type"))
			}
			if values.Get("audience") != "k8s-cluster" {
				t.Errorf("expected audience=k8s-cluster, got %s", values.Get("audience"))
			}

			resp := oidcExchangeResponse{
				AccessToken: "exchanged-k8s-token-456",
				ExpiresIn:   1800,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		provider := NewKubernetesOIDCProvider(server.URL, "k8s-cluster")
		token, err := provider.Token(context.Background(), "inbound-oidc-jwt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token != "exchanged-k8s-token-456" {
			t.Errorf("expected exchanged token, got %s", token)
		}

		// Cached fetch
		token2, err := provider.Token(context.Background(), "inbound-oidc-jwt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token2 != "exchanged-k8s-token-456" {
			t.Errorf("expected cached token, got %s", token2)
		}

		if atomic.LoadInt32(&callCount) != 1 {
			t.Errorf("expected exactly 1 exchange call, got %d", callCount)
		}
	})
}

// Mock clients to verify TokenRequest
type mockSubResourceClient struct {
	client.SubResourceClient
	onCreate func(obj client.Object, subResource client.Object) error
}

func (m *mockSubResourceClient) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	return m.onCreate(obj, subResource)
}

type mockK8sClient struct {
	client.Client
	subResourceClient client.SubResourceClient
}

func (m *mockK8sClient) SubResource(subResource string) client.SubResourceClient {
	if subResource == "token" {
		return m.subResourceClient
	}
	panic("unsupported subresource " + subResource)
}

func TestKubernetesTokenRequestProvider(t *testing.T) {
	var callCount int32
	saName := "sa-agent"
	saNamespace := "default"
	auds := []string{"https://kubernetes.default.svc"}
	var expSec int32 = 600

	const generatedToken = "generated-sa-token"

	mockSub := &mockSubResourceClient{
		onCreate: func(obj client.Object, subResource client.Object) error {
			atomic.AddInt32(&callCount, 1)

			sa, ok := obj.(*corev1.ServiceAccount)
			if !ok {
				return fmt.Errorf("expected ServiceAccount, got %T", obj)
			}
			if sa.Name != saName || sa.Namespace != saNamespace {
				return fmt.Errorf("unexpected ServiceAccount: %s/%s", sa.Namespace, sa.Name)
			}

			tr, ok := subResource.(*authenticationv1.TokenRequest)
			if !ok {
				return fmt.Errorf("expected TokenRequest, got %T", subResource)
			}

			if len(tr.Spec.Audiences) != 1 || tr.Spec.Audiences[0] != auds[0] {
				return fmt.Errorf("unexpected audiences: %v", tr.Spec.Audiences)
			}
			if *tr.Spec.ExpirationSeconds != int64(expSec) {
				return fmt.Errorf("unexpected expirationSeconds: %d", *tr.Spec.ExpirationSeconds)
			}

			tr.Status.Token = generatedToken
			tr.Status.ExpirationTimestamp = metav1.NewTime(time.Now().Add(10 * time.Minute))
			return nil
		},
	}

	mockClient := &mockK8sClient{
		subResourceClient: mockSub,
	}

	provider := NewKubernetesTokenRequestProvider(mockClient, saName, saNamespace, &expSec, auds)

	token, err := provider.Token(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != generatedToken {
		t.Errorf("expected %s, got %s", generatedToken, token)
	}

	// Verify caching
	token2, err := provider.Token(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token2 != generatedToken {
		t.Errorf("expected cached token, got %s", token2)
	}

	if atomic.LoadInt32(&callCount) != 1 {
		t.Errorf("expected exactly 1 TokenRequest call, got %d", callCount)
	}
}
