package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/agent-control-plane/aip-k8s/internal/credential"
)

var _ = Describe("Provider stub tests", func() {
	It("AzureWorkloadIdentity: client_credentials + WIF grant, exchanged token forwarded to upstream", func() {
		var capturedGrantType, capturedAssertionType, capturedAssertion string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			capturedGrantType = r.Form.Get("grant_type")
			capturedAssertionType = r.Form.Get("client_assertion_type")
			capturedAssertion = r.Form.Get("client_assertion")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"azure-access-token","expires_in":3600}`))
		}))
		defer server.Close()

		provider := credential.NewAzureWorkloadIdentityProvider("tenant-1", "client-1", "scope-1").
			WithTokenURL(server.URL)

		ctx := context.Background()
		tok, err := provider.Token(ctx, "synthetic-agent-oidc-token")
		Expect(err).NotTo(HaveOccurred())
		Expect(tok).To(Equal("azure-access-token"))
		Expect(capturedGrantType).To(Equal("client_credentials"))
		Expect(capturedAssertionType).To(Equal("urn:ietf:params:oauth:client-assertion-type:jwt-bearer"))
		Expect(capturedAssertion).To(Equal("synthetic-agent-oidc-token"))
	})

	It("AWSWebIdentity: STS AssumeRoleWithWebIdentity called, session token forwarded", func() {
		var capturedAction, capturedToken string
		var callCount int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&callCount, 1)
			_ = r.ParseForm()
			capturedAction = r.Form.Get("Action")
			capturedToken = r.Form.Get("WebIdentityToken")
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleWithWebIdentityResult>
    <Credentials>
      <AccessKeyId>ASIAIOSFODNN7EXAMPLE</AccessKeyId>
      <SecretAccessKey>wJalrXUtnFEMI/K7MDENG/bPxRfiCYzEXAMPLEKEY</SecretAccessKey>
      <SessionToken>synthetic-sts-session-token</SessionToken>
      <Expiration>2030-11-09T13:00:00Z</Expiration>
    </Credentials>
  </AssumeRoleWithWebIdentityResult>
</AssumeRoleWithWebIdentityResponse>`))
		}))
		defer server.Close()

		provider := credential.NewAWSWebIdentityProvider(
			"arn:aws:iam::123456789012:role/test-role", "session-1", "us-east-1", nil, server.URL)

		ctx := context.Background()
		tok, err := provider.Token(ctx, "synthetic-agent-oidc-token")
		Expect(err).NotTo(HaveOccurred())
		Expect(tok).To(ContainSubstring("synthetic-sts-session-token"))
		Expect(capturedAction).To(Equal("AssumeRoleWithWebIdentity"))
		Expect(capturedToken).To(Equal("synthetic-agent-oidc-token"))
		Expect(atomic.LoadInt32(&callCount)).To(Equal(int32(1)))
	})

	It("TokenCache: 10 concurrent calls to same provider deduplicate to 1 exchange", func() {
		var callCount int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&callCount, 1)
			time.Sleep(50 * time.Millisecond) // simulate delay
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"exchanged-token","expires_in":3600}`))
		}))
		defer server.Close()

		provider := credential.NewKubernetesOIDCProvider(server.URL, "kubernetes")

		ctx := context.Background()
		var wg sync.WaitGroup
		var errorsList []error
		var mu sync.Mutex
		for range 10 {
			wg.Go(func() {
				tok, err := provider.Token(ctx, "same-raw-token")
				if err != nil {
					mu.Lock()
					errorsList = append(errorsList, err)
					mu.Unlock()
				} else {
					Expect(tok).To(Equal("exchanged-token"))
				}
			})
		}
		wg.Wait()
		Expect(errorsList).To(BeEmpty())
		Expect(atomic.LoadInt64(&callCount)).To(Equal(int64(1)))
	})
})
