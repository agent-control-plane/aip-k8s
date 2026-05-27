# Phase 1 Implementation Instructions — AgentRegistration CRD + ATP Controller Bootstrap

## Context

We are adding an `AgentRegistration` CRD to AIP Kubernetes. It is the operator-owned
source of truth for an agent's identity configuration (OIDC inbound validation,
per-service outbound credential bindings). The full design is in `ep/agent-registration.md`.

**Phase 1 scope:**
1. New `AgentRegistration` CRD (`api/v1alpha1/agentregistration_types.go`)
2. `AgentTrustProfile` controller gains a watch on `AgentRegistration` → pre-creates
   the ATP proactively when a Registration appears, before any `AgentRequest` exists.
3. Helm RBAC updated for the new resource.
4. Unit tests (controller test, using the existing envtest suite in `internal/controller/`).
5. E2e tests (new Describe block or file in `test/e2e/`).

**Do NOT implement** gateway changes, credential providers, or anything from Phases 2–4.
Phase 1 is only CRD + controller bootstrap.

---

## Mandatory: Run these after all code is written

```bash
make generate     # regenerates api/v1alpha1/zz_generated.deepcopy.go
make manifests    # regenerates config/crd/bases/ and config/rbac/ from markers
make build        # must compile
make test         # unit tests must pass
```

Do not hand-edit `zz_generated.deepcopy.go`. Run `make generate` instead.

---

## File 1 — `api/v1alpha1/agentregistration_types.go` (NEW)

```go
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ExternalIdentityType identifies the credential provider for an outbound binding.
// +kubebuilder:validation:Enum=StaticSecret;AzureWorkloadIdentity;AWSWebIdentity;KubernetesOIDC
type ExternalIdentityType string

const (
	// ExternalIdentityStaticSecret uses a bearer token from a K8s Secret.
	ExternalIdentityStaticSecret ExternalIdentityType = "StaticSecret"
	// ExternalIdentityAzureWorkloadIdentity uses client_credentials + federated identity.
	ExternalIdentityAzureWorkloadIdentity ExternalIdentityType = "AzureWorkloadIdentity"
	// ExternalIdentityAWSWebIdentity uses AssumeRoleWithWebIdentity via AWS STS.
	ExternalIdentityAWSWebIdentity ExternalIdentityType = "AWSWebIdentity"
	// ExternalIdentityKubernetesOIDC uses the agent's OIDC token directly (passthrough)
	// or exchanges it via RFC 8693 for a K8s-valid token.
	ExternalIdentityKubernetesOIDC ExternalIdentityType = "KubernetesOIDC"
)

// StaticSecretCredential reads a bearer token from a K8s Secret.
type StaticSecretCredential struct {
	// Name is the Secret name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Key is the Secret data key containing the token.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
	// Namespace is the Secret namespace.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
}

// AzureWorkloadIdentityCredential configures the client credentials flow with
// federated identity for Azure Entra. The agent's OIDC token serves as the
// client_assertion proving its identity to Azure.
//
// Prerequisites (one-time operator setup per agent):
//   Create an app registration in Azure Entra for this agent. Under
//   "Certificates & secrets → Federated credentials", add a credential:
//     Issuer: <AIP gateway OIDC issuer>
//     Subject: <agentIdentity>  (must match the token's subjectClaim value)
//     Audience: api://AzureADTokenExchange
type AzureWorkloadIdentityCredential struct {
	// TenantID is the Azure Entra tenant ID.
	// +kubebuilder:validation:MinLength=1
	TenantID string `json:"tenantID"`
	// ClientID is the app registration client ID for this agent (not the AIP gateway).
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientID"`
	// Scope is the Azure resource scope, e.g. "https://app.vssps.visualstudio.com/.default".
	// +kubebuilder:validation:MinLength=1
	Scope string `json:"scope"`
}

// AWSWebIdentityCredential configures AssumeRoleWithWebIdentity via AWS STS.
// The agent's OIDC token is the WebIdentityToken passed to STS.
//
// Prerequisites (one-time operator setup per agent):
//   1. Create an IAM OIDC Identity Provider for the AIP gateway issuer.
//   2. Create an IAM role with a trust policy:
//      Action: sts:AssumeRoleWithWebIdentity
//      Condition: StringEquals <issuer>/sub = <agentIdentity>
type AWSWebIdentityCredential struct {
	// RoleARN is the IAM role ARN to assume.
	// +kubebuilder:validation:MinLength=1
	RoleARN string `json:"roleARN"`
	// RoleSessionName labels the STS session in CloudTrail.
	// +kubebuilder:validation:MinLength=1
	RoleSessionName string `json:"roleSessionName"`
	// Region is the AWS region for the STS endpoint.
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`
	// DurationSeconds is the STS session duration (default 3600, max 43200).
	// +optional
	DurationSeconds *int32 `json:"durationSeconds,omitempty"`
	// STSEndpoint overrides the default regional STS endpoint. Used in testing.
	// +optional
	STSEndpoint string `json:"stsEndpoint,omitempty"`
}

// KubernetesOIDCCredential configures OIDC token passthrough (or RFC 8693 exchange)
// for Kubernetes API servers acting as MCP server targets.
//
// Passthrough mode (TokenExchangeURL empty): the agent's validated OIDC JWT is
// forwarded directly. Requires the cluster's --oidc-issuer-url to match the AIP
// gateway issuer. K8s RBAC is enforced on the agent's own sub/groups claims.
//
// Exchange mode (TokenExchangeURL set): calls the RFC 8693 endpoint with the agent's
// JWT as subject_token. Used when gateway and K8s cluster use different issuers.
type KubernetesOIDCCredential struct {
	// TokenExchangeURL is an optional RFC 8693 token exchange endpoint.
	// When empty, the agent's JWT is forwarded to K8s directly (passthrough).
	// +optional
	TokenExchangeURL string `json:"tokenExchangeURL,omitempty"`
	// Audience overrides the aud claim for the forwarded or exchanged token.
	// +optional
	Audience string `json:"audience,omitempty"`
}

// ExternalIdentityBinding maps an MCP server to the credential provider for
// this agent on that service.
type ExternalIdentityBinding struct {
	// Service matches MCPServer.metadata.name (e.g. "github", "k8s-mcp-server").
	// +kubebuilder:validation:MinLength=1
	Service string               `json:"service"`
	// Type identifies which credential provider to use.
	Type    ExternalIdentityType `json:"type"`
	// StaticSecret is set when Type is StaticSecret.
	// +optional
	StaticSecret *StaticSecretCredential `json:"staticSecret,omitempty"`
	// AzureWorkloadIdentity is set when Type is AzureWorkloadIdentity.
	// +optional
	AzureWorkloadIdentity *AzureWorkloadIdentityCredential `json:"azureWorkloadIdentity,omitempty"`
	// AWSWebIdentity is set when Type is AWSWebIdentity.
	// +optional
	AWSWebIdentity *AWSWebIdentityCredential `json:"awsWebIdentity,omitempty"`
	// KubernetesOIDC is set when Type is KubernetesOIDC.
	// +optional
	KubernetesOIDC *KubernetesOIDCCredential `json:"kubernetesOIDC,omitempty"`
}

// AgentRegistrationOIDC declares which OIDC tokens prove this agent's identity.
type AgentRegistrationOIDC struct {
	// Issuer is the OIDC provider URL.
	// +kubebuilder:validation:MinLength=1
	Issuer string `json:"issuer"`
	// SubjectClaim is the token claim used as the agent identifier.
	// Defaults to "sub". Common alternatives: "azp" (Keycloak client_credentials),
	// "appid" (Azure AD), "email" (Google service accounts).
	// +optional
	SubjectClaim string `json:"subjectClaim,omitempty"`
	// AllowedSubjects lists token subject values that may act as this agent.
	// Supports multiple values for staging/prod variants of the same agent.
	// When empty, any subject is accepted (falls back to --agent-subjects flag).
	// +optional
	AllowedSubjects []string `json:"allowedSubjects,omitempty"`
}

// AgentRegistrationSpec defines the operator-declared identity configuration for
// an agent.
type AgentRegistrationSpec struct {
	// AgentIdentity is the canonical agent name used in AgentRequest.spec.agentIdentity,
	// GovernedResource.spec.permittedAgents, and AgentTrustProfile.spec.agentIdentity.
	// +kubebuilder:validation:MinLength=1
	AgentIdentity string `json:"agentIdentity"`

	// OIDC declares which OIDC tokens prove this agent's identity.
	// When absent, the gateway falls back to --agent-subjects flag checks.
	// +optional
	OIDC *AgentRegistrationOIDC `json:"oidc,omitempty"`

	// ExternalIdentities binds per-service outbound credentials to this agent.
	// When the gateway forwards a tool call to service X, it uses the matching
	// binding instead of the MCPServer shared token.
	// +optional
	ExternalIdentities []ExternalIdentityBinding `json:"externalIdentities,omitempty"`
}

// AgentRegistrationStatus defines the observed state of an AgentRegistration.
type AgentRegistrationStatus struct {
	// Conditions represent the latest available observations of the registration's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=areg
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentIdentity`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AgentRegistration is the operator-managed source of truth for an agent's
// identity configuration: OIDC inbound validation and per-service outbound
// credential bindings.
type AgentRegistration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentRegistrationSpec   `json:"spec,omitempty"`
	Status AgentRegistrationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentRegistrationList contains a list of AgentRegistration.
type AgentRegistrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRegistration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentRegistration{}, &AgentRegistrationList{})
}
```

---

## File 2 — `internal/controller/agenttrustprofile_controller.go` (MODIFY)

### 2a. Add the RBAC marker

Add this line immediately after the existing RBAC markers (before the `func (r *AgentTrustProfileReconciler) Reconcile` line):

```go
// +kubebuilder:rbac:groups=governance.aip.io,resources=agentregistrations,verbs=get;list;watch
```

### 2b. Modify `getOrBootstrapProfile`

In the `if !errors.IsNotFound(err)` not-found branch, ADD a new check for
`AgentRegistration` at the **top** — before the existing DiagnosticAccuracySummary
lookup. This gives Registration the highest priority for bootstrap.

Current structure (simplified):
```
if ATP not found:
  → try DiagnosticAccuracySummary
  → try AgentRequest list
  → if still nothing: return empty profile (no-op)
→ Create ATP with agentID
```

New structure:
```
if ATP not found:
  → NEW: try AgentRegistration index lookup (highest priority)
  → if not found via Registration: try DiagnosticAccuracySummary
  → try AgentRequest list
  → if still nothing: return empty profile (no-op)
→ Create ATP with agentID
```

The exact change: replace the block starting at `var agentID string` through the
existing `} else {` for the DAS lookup with the following (preserving all
existing DiagnosticAccuracySummary + AgentRequest fallback logic):

```go
var agentID string

// Priority 1: AgentRegistration (proactive, operator-declared).
// List registrations whose spec.agentIdentity maps to this profileNN.
// Uses the field index "spec.agentProfileName" set up in SetupWithManager.
var regList governancev1alpha1.AgentRegistrationList
if err := r.List(ctx, &regList,
    client.InNamespace(profileNN.Namespace),
    client.MatchingFields{"spec.agentProfileName": profileNN.Name},
); err == nil && len(regList.Items) > 0 {
    agentID = regList.Items[0].Spec.AgentIdentity
}

if agentID == "" {
    // Priority 2: DiagnosticAccuracySummary (reactive, generated by accuracy controller).
    var summary governancev1alpha1.DiagnosticAccuracySummary
    if err := r.Get(ctx, profileNN, &summary); err != nil {
        if !errors.IsNotFound(err) {
            return profile, err
        }

        // Priority 3: AgentRequest list (reactive, last-resort bootstrap).
        var arList governancev1alpha1.AgentRequestList
        if err := r.List(ctx, &arList,
            client.InNamespace(profileNN.Namespace),
            client.MatchingLabels{"aip.io/profileName": profileNN.Name},
        ); err != nil {
            return profile, err
        }
        if len(arList.Items) == 0 {
            var allARs governancev1alpha1.AgentRequestList
            if err := r.List(ctx, &allARs, client.InNamespace(profileNN.Namespace)); err != nil {
                return profile, err
            }
            for _, ar := range allARs.Items {
                if governancev1alpha1.ProfileNameForAgent(ar.Spec.AgentIdentity) == profileNN.Name {
                    agentID = ar.Spec.AgentIdentity
                    break
                }
            }
            if agentID == "" {
                return profile, nil
            }
        } else {
            agentID = arList.Items[0].Spec.AgentIdentity
        }
    } else {
        agentID = summary.Spec.AgentIdentity
    }
}
```

### 2c. Modify `SetupWithManager`

Add a field index for AgentRegistration (so the `List` in `getOrBootstrapProfile`
is efficient) and a `Watches` entry for AgentRegistration.

In `SetupWithManager`, before the `return ctrl.NewControllerManagedBy(mgr)...` block,
add:

```go
// Index AgentRegistration by the ATP name derived from spec.agentIdentity.
// This makes getOrBootstrapProfile's lookup O(1) instead of full-list.
if err := mgr.GetFieldIndexer().IndexField(ctx,
    &governancev1alpha1.AgentRegistration{},
    "spec.agentProfileName",
    func(obj client.Object) []string {
        reg, ok := obj.(*governancev1alpha1.AgentRegistration)
        if !ok || reg.Spec.AgentIdentity == "" {
            return nil
        }
        return []string{governancev1alpha1.ProfileNameForAgent(reg.Spec.AgentIdentity)}
    },
); err != nil {
    return err
}
```

**IMPORTANT:** `SetupWithManager` must receive a `ctx context.Context` parameter for
the IndexField call. Check the current signature — if it does not have `ctx`, add it.
The controller-runtime `SetupWithManager` pattern supports a context parameter:

```go
func (r *AgentTrustProfileReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
```

If the existing signature lacks `ctx`, update the signature AND update the call site
in `cmd/controller/main.go` to pass `ctx`.

Then add to the `ctrl.NewControllerManagedBy(mgr)...` chain (after the existing
`AgentGraduationPolicy` Watches block, before `.Named(...).Complete(r)`):

```go
Watches(&governancev1alpha1.AgentRegistration{},
    handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
        reg, ok := obj.(*governancev1alpha1.AgentRegistration)
        if !ok || reg.Spec.AgentIdentity == "" {
            return nil
        }
        return []reconcile.Request{{
            NamespacedName: types.NamespacedName{
                Name:      governancev1alpha1.ProfileNameForAgent(reg.Spec.AgentIdentity),
                Namespace: reg.GetNamespace(),
            },
        }}
    }),
    // Fire on create and spec changes. Not on delete — ATP persists after
    // Registration is removed so trust history is never lost.
    builder.WithPredicates(predicate.GenerationChangedPredicate{}),
).
```

---

## File 3 — `internal/controller/agenttrustprofile_controller_test.go` (MODIFY)

Add a new `Context("AgentRegistration bootstrap", ...)` block inside the existing
`Describe("AgentTrustProfile Controller", ...)`. Add these four test cases:

### Test 1: Proactive bootstrap from AgentRegistration

```go
It("bootstraps ATP when AgentRegistration is created before any AgentRequest", func() {
    agentID := "payment-bot"
    profileName := summaryNameForAgent(agentID)

    reg := &governancev1alpha1.AgentRegistration{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "payment-bot-reg",
            Namespace: ns,
        },
        Spec: governancev1alpha1.AgentRegistrationSpec{
            AgentIdentity: agentID,
        },
    }
    Expect(k8sClient.Create(ctx, reg)).To(Succeed())
    DeferCleanup(func() {
        Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, reg))).To(Succeed())
    })

    // Trigger reconcile manually (no real manager in envtest unit tests).
    r := newReconciler()
    _, err := r.Reconcile(ctx, reconcile.Request{
        NamespacedName: types.NamespacedName{Name: profileName, Namespace: ns},
    })
    Expect(err).NotTo(HaveOccurred())

    var atp governancev1alpha1.AgentTrustProfile
    Expect(k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)).To(Succeed())
    Expect(atp.Spec.AgentIdentity).To(Equal(agentID))
})
```

### Test 2: AgentRegistration takes precedence over DiagnosticAccuracySummary

```go
It("uses AgentRegistration identity over DiagnosticAccuracySummary when both exist", func() {
    agentID := "reg-wins-agent"
    differentID := "das-agent-different"
    profileName := summaryNameForAgent(agentID)

    // Create a DAS with a different identity that maps to the same profile name.
    // (contrived, but verifies priority ordering.)
    das := &governancev1alpha1.DiagnosticAccuracySummary{
        ObjectMeta: metav1.ObjectMeta{Name: profileName, Namespace: ns},
        Spec:       governancev1alpha1.DiagnosticAccuracySummarySpec{AgentIdentity: differentID},
    }
    Expect(k8sClient.Create(ctx, das)).To(Succeed())
    DeferCleanup(func() {
        Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, das))).To(Succeed())
    })

    reg := &governancev1alpha1.AgentRegistration{
        ObjectMeta: metav1.ObjectMeta{Name: "reg-wins", Namespace: ns},
        Spec:       governancev1alpha1.AgentRegistrationSpec{AgentIdentity: agentID},
    }
    Expect(k8sClient.Create(ctx, reg)).To(Succeed())
    DeferCleanup(func() {
        Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, reg))).To(Succeed())
    })

    r := newReconciler()
    _, err := r.Reconcile(ctx, reconcile.Request{
        NamespacedName: types.NamespacedName{Name: profileName, Namespace: ns},
    })
    Expect(err).NotTo(HaveOccurred())

    var atp governancev1alpha1.AgentTrustProfile
    Expect(k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)).To(Succeed())
    // AgentRegistration identity wins.
    Expect(atp.Spec.AgentIdentity).To(Equal(agentID))
})
```

### Test 3: Backward compat — reactive bootstrap still works without Registration

```go
It("falls back to AgentRequest bootstrap when no AgentRegistration exists", func() {
    agentID := "legacy-agent-no-reg"
    profileName := summaryNameForAgent(agentID)

    ar := &governancev1alpha1.AgentRequest{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "ar-bootstrap",
            Namespace: ns,
            Labels:    map[string]string{"aip.io/profileName": profileName},
        },
        Spec: governancev1alpha1.AgentRequestSpec{AgentIdentity: agentID, TargetURI: "k8s://x"},
    }
    Expect(k8sClient.Create(ctx, ar)).To(Succeed())
    DeferCleanup(func() {
        Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ar))).To(Succeed())
    })

    r := newReconciler()
    _, err := r.Reconcile(ctx, reconcile.Request{
        NamespacedName: types.NamespacedName{Name: profileName, Namespace: ns},
    })
    Expect(err).NotTo(HaveOccurred())

    var atp governancev1alpha1.AgentTrustProfile
    Expect(k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)).To(Succeed())
    Expect(atp.Spec.AgentIdentity).To(Equal(agentID))
})
```

### Test 4: Deleting Registration does NOT delete ATP

```go
It("preserves ATP when AgentRegistration is deleted", func() {
    agentID := "preserved-agent"
    profileName := summaryNameForAgent(agentID)

    reg := &governancev1alpha1.AgentRegistration{
        ObjectMeta: metav1.ObjectMeta{Name: "to-delete-reg", Namespace: ns},
        Spec:       governancev1alpha1.AgentRegistrationSpec{AgentIdentity: agentID},
    }
    Expect(k8sClient.Create(ctx, reg)).To(Succeed())

    // Bootstrap the ATP.
    r := newReconciler()
    _, err := r.Reconcile(ctx, reconcile.Request{
        NamespacedName: types.NamespacedName{Name: profileName, Namespace: ns},
    })
    Expect(err).NotTo(HaveOccurred())

    var atp governancev1alpha1.AgentTrustProfile
    Expect(k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)).To(Succeed())

    // Delete Registration.
    Expect(k8sClient.Delete(ctx, reg)).To(Succeed())

    // ATP must still exist.
    Expect(k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)).To(Succeed())
    DeferCleanup(func() {
        Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &atp))).To(Succeed())
    })
})
```

**IMPORTANT for tests:** The field index set up in `SetupWithManager` is NOT available
in the envtest unit tests that call `r.Reconcile(...)` directly (no manager running).
To handle this, `getOrBootstrapProfile` must handle the case where the field index
returns an error by falling through gracefully. Alternatively, make the indexer optional:
if the `List` with `MatchingFields` returns an error, fall through to the existing
DAS/AgentRequest cascade. This makes the unit tests work without an index while the
real controller uses the indexed path.

Adjust the `List` call:
```go
var regList governancev1alpha1.AgentRegistrationList
if err := r.List(ctx, &regList,
    client.InNamespace(profileNN.Namespace),
    client.MatchingFields{"spec.agentProfileName": profileNN.Name},
); err == nil && len(regList.Items) > 0 {
    agentID = regList.Items[0].Spec.AgentIdentity
}
// If err != nil (e.g. index not registered in unit tests), agentID stays ""
// and we fall through to DiagnosticAccuracySummary / AgentRequest cascade.
```

---

## File 4 — E2e tests: `test/e2e/agent_registration_test.go` (NEW)

Create a new file for Phase 1 e2e tests. These run against a real Kind cluster.
Follow the patterns in `test/e2e/e2e_test.go` and `test/e2e/e2e_suite_test.go`.

```go
package e2e

import (
    "time"

    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"
    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/types"
    "sigs.k8s.io/controller-runtime/pkg/client"

    governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

var _ = Describe("AgentRegistration", Ordered, func() {
    var ns string

    BeforeAll(func() {
        // Create a unique namespace for this Describe block.
        ns = "e2e-areg-" + randomSuffix()
        Expect(k8sClient.Create(ctx, &corev1.Namespace{
            ObjectMeta: metav1.ObjectMeta{Name: ns},
        })).To(Succeed())
    })

    AfterAll(func() {
        // Clean up all resources in this namespace.
        Expect(k8sClient.DeleteAllOf(ctx, &governancev1alpha1.AgentRegistration{},
            client.InNamespace(ns))).To(Succeed())
        Expect(k8sClient.DeleteAllOf(ctx, &governancev1alpha1.AgentTrustProfile{},
            client.InNamespace(ns))).To(Succeed())
        Expect(k8sClient.Delete(ctx, &corev1.Namespace{
            ObjectMeta: metav1.ObjectMeta{Name: ns},
        })).To(Succeed())
    })

    It("controller creates ATP when AgentRegistration is applied", func() {
        agentID := "e2e-payment-bot"
        reg := &governancev1alpha1.AgentRegistration{
            ObjectMeta: metav1.ObjectMeta{
                Name:      "payment-bot",
                Namespace: ns,
            },
            Spec: governancev1alpha1.AgentRegistrationSpec{
                AgentIdentity: agentID,
                OIDC: &governancev1alpha1.AgentRegistrationOIDC{
                    Issuer:          "https://keycloak.example.com/realms/aip",
                    SubjectClaim:    "azp",
                    AllowedSubjects: []string{"payment-bot"},
                },
            },
        }
        Expect(k8sClient.Create(ctx, reg)).To(Succeed())

        profileName := governancev1alpha1.ProfileNameForAgent(agentID)
        Eventually(func() error {
            var atp governancev1alpha1.AgentTrustProfile
            return k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)
        }, 30*time.Second, time.Second).Should(Succeed(), "ATP should be created by controller")

        var atp governancev1alpha1.AgentTrustProfile
        Expect(k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)).To(Succeed())
        Expect(atp.Spec.AgentIdentity).To(Equal(agentID))
    })

    It("ATP persists after AgentRegistration is deleted", func() {
        agentID := "e2e-payment-bot" // same agent from previous It
        profileName := governancev1alpha1.ProfileNameForAgent(agentID)

        // Delete the registration.
        reg := &governancev1alpha1.AgentRegistration{}
        Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "payment-bot", Namespace: ns}, reg)).To(Succeed())
        Expect(k8sClient.Delete(ctx, reg)).To(Succeed())

        // ATP must still exist after deletion.
        Consistently(func() error {
            var atp governancev1alpha1.AgentTrustProfile
            return k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)
        }, 5*time.Second, time.Second).Should(Succeed(), "ATP must not be deleted when Registration is removed")
    })

    It("ATP is still bootstrapped reactively from AgentRequest when no Registration exists", func() {
        agentID := "e2e-legacy-no-reg"
        profileName := governancev1alpha1.ProfileNameForAgent(agentID)

        // Create an AgentRequest with no Registration.
        ar := &governancev1alpha1.AgentRequest{
            ObjectMeta: metav1.ObjectMeta{
                GenerateName: "ar-legacy-",
                Namespace:    ns,
            },
            Spec: governancev1alpha1.AgentRequestSpec{
                AgentIdentity: agentID,
                TargetURI:     "k8s://test/resource",
                Action:        "read",
            },
        }
        Expect(k8sClient.Create(ctx, ar)).To(Succeed())

        Eventually(func() error {
            var atp governancev1alpha1.AgentTrustProfile
            return k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)
        }, 30*time.Second, time.Second).Should(Succeed(), "ATP should be bootstrapped reactively from AgentRequest")
    })
})
```

**Note:** If a `randomSuffix()` helper does not exist in the e2e suite, use:
```go
import "fmt"
func randomSuffix() string {
    return fmt.Sprintf("%d", time.Now().UnixNano()%100000)
}
```
or check if there is an existing helper in `e2e_suite_test.go`.

---

## File 5 — `charts/aip-k8s/templates/controller/rbac.yaml` (MODIFY)

In the `ClusterRole` rules section, find the existing block for `governance.aip.io`
resources that lists multiple resources. Add `agentregistrations` to the list
(keep alphabetical order):

```yaml
- apiGroups:
  - governance.aip.io
  resources:
  - agentgraduationpolicies
  - agentregistrations          # ← ADD THIS LINE
  - agentrequests
  - agenttrustprofiles
  - auditrecords
  - diagnosticaccuracysummaries
  - governedresources
  - safetypolicies
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
```

**Note:** The controller only needs `get;list;watch` on AgentRegistration (it never
creates or mutates them — they are operator-managed). If you want to be more
restrictive (least-privilege), add a separate rule:

```yaml
- apiGroups:
  - governance.aip.io
  resources:
  - agentregistrations
  verbs:
  - get
  - list
  - watch
```

Use the separate rule (least-privilege) to keep the RBAC tight.

---

## Verification checklist

After implementing all changes:

```bash
make generate      # Must produce updated zz_generated.deepcopy.go with AgentRegistration methods
make manifests     # Must produce config/crd/bases/governance.aip.io_agentregistrations.yaml
make build         # Must compile without errors
make test          # Unit tests must pass, including the 4 new AgentRegistration bootstrap tests
make lint          # No lint errors
```

Check that `zz_generated.deepcopy.go` contains `DeepCopyObject` for `AgentRegistration`
and `AgentRegistrationList`, and `DeepCopyInto` for all new struct types.

---

## Common mistakes to avoid

1. **Do NOT hand-edit `zz_generated.deepcopy.go`** — run `make generate`.
2. **Do NOT add gateway changes** — Phase 1 is CRD + controller only.
3. **Do NOT delete ATP when AgentRegistration is deleted** — no owner reference,
   no finalizer on Registration. ATP persists independently.
4. **The field index `spec.agentProfileName` must be set up in `SetupWithManager`**
   using `mgr.GetFieldIndexer().IndexField(ctx, ...)`. The unit tests bypass this
   (no manager), so the `List` in `getOrBootstrapProfile` must silently fall through
   on index errors.
5. **`AlreadyExists` must be handled** in the `Create(ATP)` path —
   `errors.IsAlreadyExists(err)` is already handled in the existing code; preserve it.
6. **RBAC marker + Helm RBAC must both be updated.** Run `make manifests` after
   adding the kubebuilder RBAC marker to regenerate `config/rbac/`.
