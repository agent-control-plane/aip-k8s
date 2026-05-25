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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MCPServerSpec defines the desired state of an upstream MCP server registration.
type MCPServerSpec struct {
	// URL is the base URL of the upstream MCP server.
	// The gateway uses this directly for MCP protocol calls.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url"`

	// SecretNamespace specifies the namespace of the Secret referenced by
	// BearerTokenSecretRef. Required when bearerTokenSecretRef is set.
	// +optional
	SecretNamespace string `json:"secretNamespace,omitempty"`

	// BearerTokenSecretRef references a Secret containing the bearer token
	// for authenticating to the upstream MCP server.
	// The Secret key is specified by the Key field of the selector.
	// +optional
	BearerTokenSecretRef *corev1.SecretKeySelector `json:"bearerTokenSecretRef,omitempty"`

	// ReadOnlyTools lists tool names that should be treated as read-only.
	// Tools not in this list default to read-write.
	// +optional
	ReadOnlyTools []string `json:"readOnlyTools,omitempty"`
}

// MCPServerStatus defines the observed state of an MCPServer.
type MCPServerStatus struct {
	// LastSyncTime is the last time the controller successfully synced tools.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// ObservedGeneration is the generation last observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest state of the MCPServer.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// DiscoveredToolCount is the number of tools discovered from upstream.
	// +optional
	DiscoveredToolCount int `json:"discoveredToolCount,omitempty"`

	// Tools is the list of tools discovered from the upstream MCP server.
	// +optional
	Tools []MCPServerTool `json:"tools,omitempty"`
}

// MCPServerTool describes a single tool exposed by the upstream MCP server.
type MCPServerTool struct {
	// Name is the bare tool name (e.g. "create_pull_request").
	Name string `json:"name"`

	// ReadOnly indicates whether this tool is read-only.
	ReadOnly bool `json:"readOnly"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=='Synced')].status`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Tools",type=integer,JSONPath=`.status.discoveredToolCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="!(has(self.spec.bearerTokenSecretRef) && self.spec.secretNamespace == '')",message="secretNamespace is required when bearerTokenSecretRef is set"

// MCPServer is the Schema for registering upstream MCP servers.
type MCPServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec   MCPServerSpec   `json:"spec"`
	Status MCPServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MCPServerList contains a list of MCPServer.
type MCPServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MCPServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MCPServer{}, &MCPServerList{})
}
