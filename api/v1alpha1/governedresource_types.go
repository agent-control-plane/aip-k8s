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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const GovernedResourceFinalizer = "governance.aip.io/active-requests"

// GovernedResourceSpec defines the desired state of GovernedResource
type GovernedResourceSpec struct {
	// URIPattern is a glob pattern matched against AgentRequest.spec.target.uri.
	// Requests targeting URIs that do not match any GovernedResource are rejected.
	// Examples:
	//   "k8s://prod/karpenter/nodepool/team-a-*"
	//   "github://org/repo-*"
	//   "k8s://*/default/deployment/*"
	// +kubebuilder:validation:MinLength=1
	URIPattern string `json:"uriPattern"`

	// PermittedActions lists the action strings agents may request on this resource.
	// Requests with actions not in this list are rejected.
	// +kubebuilder:validation:MinItems=1
	PermittedActions []string `json:"permittedActions"`

	// PermittedAgents lists agent identity values (matched against --oidc-identity-claim)
	// that may submit AgentRequests targeting this resource.
	// Empty means any authenticated agent may target this resource.
	// +optional
	PermittedAgents []string `json:"permittedAgents,omitempty"`

	// ContextFetcher names the built-in fetcher to invoke when an AgentRequest
	// targets this resource type. The fetcher populates status.providerContext
	// so reviewers see live resource state alongside the agent's declared intent.
	// Supported values: "karpenter", "github", "k8s-deployment", "none"
	// +kubebuilder:validation:Enum=karpenter;github;k8s-deployment;none
	// +kubebuilder:default=none
	ContextFetcher string `json:"contextFetcher"`

	// ContextSchema is an OpenAPI schema (restricted subset) describing the JSON
	// object the ContextFetcher returns. SafetyPolicy CEL rules that reference
	// context.* fields are type-checked against this schema at policy creation time.
	//
	// Restricted subset: type (object/string/integer/number/boolean/array),
	// properties, items, required, nullable, description only. All other keywords
	// are rejected at admission.
	//
	// May be omitted when ContextFetcher is "none".
	// +optional
	ContextSchema *apiextensionsv1.JSON `json:"contextSchema,omitempty"`

	// Description is a human-readable explanation of this governed resource type,
	// shown to reviewers during the approval decision.
	// +optional
	Description string `json:"description,omitempty"`

	// SoakMode routes all incoming AgentRequests to AwaitingVerdict phase
	// instead of Pending. Use during initial agent validation to grade
	// diagnostic quality before allowing real execution.
	// Flip to false when the admin has confidence in the agent.
	// +optional
	SoakMode bool `json:"soakMode,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="URIPattern",type=string,JSONPath=`.spec.uriPattern`
// +kubebuilder:printcolumn:name="Fetcher",type=string,JSONPath=`.spec.contextFetcher`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// GovernedResource is the Schema for the governedresources API
type GovernedResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GovernedResourceSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// GovernedResourceList contains a list of GovernedResource
type GovernedResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GovernedResource `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GovernedResource{}, &GovernedResourceList{})
}
