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

// AuditRecordSpec defines the desired state of AuditRecord
type AuditRecordSpec struct {
	Timestamp metav1.Time `json:"timestamp"` // RFC 3339
	// +kubebuilder:validation:MinLength=1
	AgentIdentity string `json:"agentIdentity"`
	// +kubebuilder:validation:MinLength=1
	AgentRequestRef string `json:"agentRequestRef"` // Name of the AgentRequest CR

	// Event classification
	// +kubebuilder:validation:Enum="request.submitted";"request.approved";"request.denied";"request.executing";"request.completed";"request.failed";"request.revoked";"lock.acquired";"lock.released";"lock.expired";"policy.evaluated";"cascade.mismatch";"heartbeat.timeout";"state.drifted";"state.drifted.verify"
	Event string `json:"event"`

	// AgentRequest snapshot at the time of the event
	// +kubebuilder:validation:MinLength=1
	Action string `json:"action"`
	// +kubebuilder:validation:MinLength=1
	TargetURI string `json:"targetURI"`
	Reason    string `json:"reason"`

	// Transition details
	PhaseTransition *PhaseTransition `json:"phaseTransition,omitempty"`

	// Policy evaluation outcomes
	PolicyEvaluations []AuditPolicyEvaluation `json:"policyEvaluations,omitempty"`

	// Lock state at the time of the event
	LockStatus *AuditLockStatus `json:"lockStatus,omitempty"`

	// Extensible metadata (e.g., CASCADE_MISMATCH details)
	Annotations map[string]string `json:"annotations,omitempty"`

	// Event-specific details (e.g., denial reason, heartbeat data)
	Details *apiextensionsv1.JSON `json:"details,omitempty"`
}

// Audit event types (from AIP spec Section 9.3)
const (
	AuditEventRequestSubmitted   = "request.submitted"
	AuditEventRequestApproved    = "request.approved"
	AuditEventRequestDenied      = "request.denied"
	AuditEventRequestExecuting   = "request.executing"
	AuditEventRequestCompleted   = "request.completed"
	AuditEventRequestFailed      = "request.failed"
	AuditEventRequestRevoked     = "request.revoked"
	AuditEventLockAcquired       = "lock.acquired"
	AuditEventLockReleased       = "lock.released"
	AuditEventLockExpired        = "lock.expired"
	AuditEventPolicyEvaluated    = "policy.evaluated"
	AuditEventCascadeMismatch    = "cascade.mismatch"
	AuditEventHeartbeatTimeout   = "heartbeat.timeout"
	AuditEventStateDrifted       = "state.drifted"
	AuditEventStateDriftedVerify = "state.drifted.verify"
)

type PhaseTransition struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type AuditPolicyEvaluation struct {
	PolicyName string `json:"policyName"`
	RuleName   string `json:"ruleName"`
	Result     string `json:"result"` // "Allow", "Deny", "Log", "RequireApproval", "EvaluationFailed"
	Reason     string `json:"reason,omitempty"`
}

type AuditLockStatus struct {
	LeaseName string `json:"leaseName"`
	TargetURI string `json:"targetURI"`
	Event     string `json:"event"` // "acquired", "released", "expired", "contention"
}

// AuditRecordStatus defines the observed state of AuditRecord.
type AuditRecordStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Event",type=string,JSONPath=`.spec.event`
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentIdentity`
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetURI`,priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AuditRecord is the Schema for the auditrecords API
type AuditRecord struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of AuditRecord
	// +required
	Spec AuditRecordSpec `json:"spec"`

	// status defines the observed state of AuditRecord
	// +optional
	Status AuditRecordStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AuditRecordList contains a list of AuditRecord
type AuditRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AuditRecord `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AuditRecord{}, &AuditRecordList{})
}
