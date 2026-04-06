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

// SafetyPolicySpec defines the desired state of SafetyPolicy
type SafetyPolicySpec struct {
	// GovernedResourceSelector selects which GovernedResources this policy applies to.
	// An empty selector matches all GovernedResources in scope.
	// +optional
	GovernedResourceSelector metav1.LabelSelector `json:"governedResourceSelector,omitempty"`

	// ContextType binds CEL rules that reference context.* fields to a specific
	// context fetcher type. Must match a GovernedResource.spec.contextFetcher value.
	// Empty means no context-aware rules are permitted in this policy.
	// +optional
	ContextType string `json:"contextType,omitempty"`

	// +kubebuilder:validation:MinItems=1
	Rules []Rule `json:"rules"`
	// +kubebuilder:validation:Enum=FailClosed;FailOpen
	// +kubebuilder:default=FailClosed
	FailureMode *string `json:"failureMode,omitempty"`
}

type Rule struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=StateEvaluation;TimeWindow;RateLimit
	Type string `json:"type"`
	// +kubebuilder:validation:Enum=Allow;Deny;Log;RequireApproval
	Action  string  `json:"action"`
	Message *string `json:"message,omitempty"` // Human-readable explanation
	// +kubebuilder:validation:MinLength=1
	Expression string                `json:"expression"`       // CEL expression
	Config     *apiextensionsv1.JSON `json:"config,omitempty"` // Type-specific configuration
}

// SafetyPolicyStatus defines the observed state of SafetyPolicy.
type SafetyPolicyStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="FailureMode",type=string,JSONPath=`.spec.failureMode`
// +kubebuilder:printcolumn:name="Rules",type=integer,JSONPath=`.spec.rules[*].name`,priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SafetyPolicy is the Schema for the safetypolicies API
type SafetyPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SafetyPolicy
	// +required
	Spec SafetyPolicySpec `json:"spec"`

	// status defines the observed state of SafetyPolicy
	// +optional
	Status SafetyPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SafetyPolicyList contains a list of SafetyPolicy
type SafetyPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SafetyPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SafetyPolicy{}, &SafetyPolicyList{})
}
