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

// Registration condition types
const (
	// ConditionServiceBindingInert is True when an externalIdentities binding
	// exists for a service that is not in status.approvedServices or no longer
	// exists in the MCPServer catalog.
	ConditionServiceBindingInert = "ServiceBindingInert"
)

// Reasons for ConditionServiceBindingInert
const (
	// ReasonServiceNotApproved means the service was not included in the
	// admin-approved subset of requestedServices.
	ReasonServiceNotApproved = "ServiceNotApproved"
	// ReasonServiceNotFound means the MCPServer referenced in the binding
	// no longer exists in the catalog.
	ReasonServiceNotFound = "ServiceNotFound"
)

// ExternalIdentityType identifies the credential provider for an outbound binding.
// +kubebuilder:validation:Enum=StaticSecret;AzureWorkloadIdentity;AWSWebIdentity;KubernetesOIDC;KubernetesTokenRequest
type ExternalIdentityType string

const (
	// ExternalIdentityStaticSecret uses a bearer token from a K8s Secret.
	ExternalIdentityStaticSecret ExternalIdentityType = "StaticSecret"
	// ExternalIdentityAzureWorkloadIdentity uses client_credentials + federated identity.
	ExternalIdentityAzureWorkloadIdentity ExternalIdentityType = "AzureWorkloadIdentity"
	// ExternalIdentityAWSWebIdentity uses AssumeRoleWithWebIdentity via AWS STS.
	ExternalIdentityAWSWebIdentity ExternalIdentityType = "AWSWebIdentity"
	// ExternalIdentityKubernetesOIDC exchanges the agent's OIDC token via RFC 8693
	// for a target-audience token. Passthrough mode is not supported.
	ExternalIdentityKubernetesOIDC ExternalIdentityType = "KubernetesOIDC"
	// ExternalIdentityKubernetesTokenRequest uses K8s ServiceAccount TokenRequest API.
	ExternalIdentityKubernetesTokenRequest ExternalIdentityType = "KubernetesTokenRequest"
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
//
//	Create an app registration in Azure Entra for this agent. Under
//	"Certificates & secrets → Federated credentials", add a credential:
//	  Issuer: <AIP gateway OIDC issuer>
//	  Subject: <agentIdentity>  (must match the token's subjectClaim value)
//	  Audience: api://AzureADTokenExchange
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
//
//  1. Create an IAM OIDC Identity Provider for the AIP gateway issuer.
//  2. Create an IAM role with a trust policy:
//     Action: sts:AssumeRoleWithWebIdentity
//     Condition: StringEquals <issuer>/sub = <agentIdentity>
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

// KubernetesOIDCCredential configures RFC 8693 token exchange for Kubernetes API
// servers acting as MCP server targets. The agent's validated OIDC JWT is exchanged
// for a target-audience token at the configured endpoint.
//
// Passthrough mode (no exchange) is intentionally not supported: forwarding a
// gateway-audience token to upstream servers allows a compromised server to replay
// it against the gateway. Always configure a dedicated token exchange endpoint.
type KubernetesOIDCCredential struct {
	// TokenExchangeURL is the RFC 8693 token exchange endpoint.
	// +kubebuilder:validation:MinLength=1
	TokenExchangeURL string `json:"tokenExchangeURL"`
	// Audience overrides the aud claim for the forwarded or exchanged token.
	// +optional
	Audience string `json:"audience,omitempty"`
}

// KubernetesTokenRequestCredential configures using K8s ServiceAccount TokenRequest API.
type KubernetesTokenRequestCredential struct {
	// ServiceAccountName is the name of the ServiceAccount to impersonate.
	// +kubebuilder:validation:MinLength=1
	ServiceAccountName string `json:"serviceAccountName"`
	// ServiceAccountNamespace is the namespace of the ServiceAccount.
	// +kubebuilder:validation:MinLength=1
	ServiceAccountNamespace string `json:"serviceAccountNamespace"`
	// ExpirationSeconds is the requested duration of validity for the token.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ExpirationSeconds *int32 `json:"expirationSeconds,omitempty"`
	// Audiences are the intended audiences of the token.
	// +optional
	Audiences []string `json:"audiences,omitempty"`
}

// ExternalIdentityBinding maps an MCP server to the credential provider for
// this agent on that service.
// +kubebuilder:validation:XValidation:rule="self.type == 'StaticSecret' ? has(self.staticSecret) : !has(self.staticSecret)",message="staticSecret must be set when type is StaticSecret and unset otherwise"
// +kubebuilder:validation:XValidation:rule="self.type == 'AzureWorkloadIdentity' ? has(self.azureWorkloadIdentity) : !has(self.azureWorkloadIdentity)",message="azureWorkloadIdentity must be set when type is AzureWorkloadIdentity and unset otherwise"
// +kubebuilder:validation:XValidation:rule="self.type == 'AWSWebIdentity' ? has(self.awsWebIdentity) : !has(self.awsWebIdentity)",message="awsWebIdentity must be set when type is AWSWebIdentity and unset otherwise"
// +kubebuilder:validation:XValidation:rule="self.type == 'KubernetesOIDC' ? has(self.kubernetesOIDC) : !has(self.kubernetesOIDC)",message="kubernetesOIDC must be set when type is KubernetesOIDC and unset otherwise"
// +kubebuilder:validation:XValidation:rule="self.type == 'KubernetesTokenRequest' ? has(self.kubernetesTokenRequest) : !has(self.kubernetesTokenRequest)",message="kubernetesTokenRequest must be set when type is KubernetesTokenRequest and unset otherwise"
type ExternalIdentityBinding struct {
	// Service matches MCPServer.metadata.name (e.g. "github", "k8s-mcp-server").
	// +kubebuilder:validation:MinLength=1
	Service string `json:"service"`
	// Type identifies which credential provider to use.
	Type ExternalIdentityType `json:"type"`
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
	// KubernetesTokenRequest is set when Type is KubernetesTokenRequest.
	// +optional
	KubernetesTokenRequest *KubernetesTokenRequestCredential `json:"kubernetesTokenRequest,omitempty"`
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

// AgentRegistrationMode selects the credential posture for this agent.
// +kubebuilder:validation:Enum=Standing;Governed
type AgentRegistrationMode string

const (
	// AgentRegistrationModeStanding means the agent keeps its existing access;
	// AIP provides attribution, shadow policies, and audit. This is the default
	// posture — registration must never change an existing agent's behavior
	// without explicit opt-in (design principle 1: the affected party consents).
	// A Governed default would mean the act of registering cuts off an agent's
	// write path — imposed, not chosen. Defaulting to Standing keeps enrollment
	// a zero-risk observation step; the developer requests Governed, the admin
	// countersigns. This is a considered decision, not an oversight.
	AgentRegistrationModeStanding AgentRegistrationMode = "Standing"
	// AgentRegistrationModeGoverned means writes flow through approved intents
	// and JIT-minted tokens. The gateway is the only write path.
	AgentRegistrationModeGoverned AgentRegistrationMode = "Governed"
)

// AgentRegistrationSpec defines the operator-declared identity configuration for
// an agent.
type AgentRegistrationSpec struct {
	// AgentIdentity is the canonical agent name used in AgentRequest.spec.agentIdentity,
	// GovernedResource.spec.permittedAgents, and AgentTrustProfile.spec.agentIdentity.
	// +kubebuilder:validation:MinLength=1
	AgentIdentity string `json:"agentIdentity"`

	// Mode selects the credential posture for this agent.
	//   "Standing" (default) — agent keeps its existing access; AIP provides
	//   attribution, shadow policies, and audit. Pure addition, zero risk.
	//   "Governed" — writes flow through approved intents + JIT-minted tokens.
	// +kubebuilder:default=Standing
	// +optional
	Mode AgentRegistrationMode `json:"mode,omitempty"`

	// RequestedServices lists the services the agent wants credential
	// bindings for (e.g. "k8s", "github"). Admin may approve a subset.
	// +optional
	RequestedServices []string `json:"requestedServices,omitempty"`

	// OIDC declares which OIDC tokens prove this agent's identity.
	// When absent, the gateway falls back to --agent-subjects flag checks.
	// +optional
	OIDC *AgentRegistrationOIDC `json:"oidc,omitempty"`

	// ExternalIdentities binds per-service outbound credentials to this agent.
	// When the gateway forwards a tool call to service X, it uses the matching
	// binding instead of the MCPServer shared token.
	// +optional
	// +listType=map
	// +listMapKey=service
	ExternalIdentities []ExternalIdentityBinding `json:"externalIdentities,omitempty"`
}

// AgentRegistrationStatus defines the observed state of an AgentRegistration.
type AgentRegistrationStatus struct {
	// Phase is the lifecycle phase: "" | Pending | Approved | Denied.
	// +optional
	Phase string `json:"phase,omitempty"`

	// RegisteredBy records the human identity whose login submitted this
	// registration. Set by the gateway from the authenticated caller at
	// creation time; immutable thereafter.
	// +optional
	RegisteredBy string `json:"registeredBy,omitempty"`

	// ApprovedServices is the admin-confirmed subset of spec.requestedServices.
	// Under auto policy it equals requestedServices. Under manual policy the
	// admin may approve a subset.
	// +optional
	ApprovedServices []string `json:"approvedServices,omitempty"`

	// ApprovedAt records when the registration last transitioned to Approved.
	// Drives --registration-max-age re-attestation.
	// +optional
	ApprovedAt *metav1.Time `json:"approvedAt,omitempty"`

	// Conditions represent the latest available observations of the registration's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=areg
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentIdentity`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AgentRegistration is the operator-managed source of truth for an agent's
// identity configuration: OIDC inbound validation and per-service outbound
// credential bindings.
// +kubebuilder:validation:XValidation:rule="has(self.spec) && has(self.spec.agentIdentity)",message="spec.agentIdentity is required"
type AgentRegistration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec   AgentRegistrationSpec   `json:"spec"`
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
