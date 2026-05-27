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
