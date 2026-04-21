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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// DiagnosticAccuracySummarySpec defines the desired state of DiagnosticAccuracySummary
type DiagnosticAccuracySummarySpec struct {
	// AgentIdentity specifies the agent this summary tracks.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	AgentIdentity string `json:"agentIdentity"`
}

// DiagnosticAccuracySummaryStatus defines the observed state of DiagnosticAccuracySummary.
type DiagnosticAccuracySummaryStatus struct {
	// TotalReviewed is the total number of AgentDiagnostic records
	// with a non-empty verdict for this agentIdentity.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TotalReviewed int64 `json:"totalReviewed,omitempty"`

	// CorrectCount is the number of verdicts set to "correct".
	// +kubebuilder:validation:Minimum=0
	// +optional
	CorrectCount int64 `json:"correctCount,omitempty"`

	// PartialCount is the number of verdicts set to "partial".
	// +kubebuilder:validation:Minimum=0
	// +optional
	PartialCount int64 `json:"partialCount,omitempty"`

	// IncorrectCount is the number of verdicts set to "incorrect".
	// +kubebuilder:validation:Minimum=0
	// +optional
	IncorrectCount int64 `json:"incorrectCount,omitempty"`

	// DiagnosticAccuracy is the computed accuracy ratio:
	//   (correctCount + 0.5 * partialCount) / totalReviewed
	// Null if totalReviewed == 0.
	// +optional
	DiagnosticAccuracy *float64 `json:"diagnosticAccuracy,omitempty"`

	// LastUpdatedAt is the timestamp of the most recent verdict that
	// contributed to this summary.
	// +optional
	LastUpdatedAt *metav1.Time `json:"lastUpdatedAt,omitempty"`

	// RecentVerdicts stores the names of the most recent AgentRequests
	// that have been counted in this summary, to prevent double-counting.
	// +optional
	// +kubebuilder:validation:MaxItems=100
	RecentVerdicts []string `json:"recentVerdicts,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced

// DiagnosticAccuracySummary is the Schema for the diagnosticaccuracysummaries API
type DiagnosticAccuracySummary struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of DiagnosticAccuracySummary
	// +required
	Spec DiagnosticAccuracySummarySpec `json:"spec"`

	// status defines the observed state of DiagnosticAccuracySummary
	// +optional
	Status DiagnosticAccuracySummaryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DiagnosticAccuracySummaryList contains a list of DiagnosticAccuracySummary
type DiagnosticAccuracySummaryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DiagnosticAccuracySummary `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DiagnosticAccuracySummary{}, &DiagnosticAccuracySummaryList{})
}
