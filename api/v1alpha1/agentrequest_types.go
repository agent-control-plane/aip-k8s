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

// AgentRequestSpec defines the desired state of AgentRequest
type AgentRequestSpec struct {
	// +kubebuilder:validation:MinLength=1
	AgentIdentity string `json:"agentIdentity"` // Mutated/Validated by Webhook
	// +kubebuilder:validation:MinLength=1
	Action string `json:"action"` // Must be mutating
	Target Target `json:"target"`
	// +kubebuilder:validation:MinLength=1
	Reason string `json:"reason"`

	// Optional fields
	IntentPlanRef *string `json:"intentPlanRef,omitempty"` // Reference to parent IntentPlan
	// +kubebuilder:validation:Minimum=0
	Priority         *int32                `json:"priority,omitempty"`
	CascadeModel     *CascadeModel         `json:"cascadeModel,omitempty"`
	ReasoningTrace   *ReasoningTrace       `json:"reasoningTrace,omitempty"`
	Interruptibility *bool                 `json:"interruptibility,omitempty"` // Default: false
	Parameters       *apiextensionsv1.JSON `json:"parameters,omitempty"`       // Action-specific parameters

	// ExecutionMode declares how the agent plans to execute.
	// "single" (default): a specific pre-declared action.
	// "scoped": dynamic execution within declared bounds (e.g. ReAct / Bedrock agents).
	// When "scoped", ScopeBounds MUST also be provided.
	// +kubebuilder:validation:Enum=single;scoped
	// +kubebuilder:default=single
	ExecutionMode *string      `json:"executionMode,omitempty"`
	ScopeBounds   *ScopeBounds `json:"scopeBounds,omitempty"`

	// HumanApproval holds the human reviewer's decision when a policy requires manual approval.
	// The controller watches this field and drives the status state machine accordingly.
	// +optional
	HumanApproval *HumanApproval `json:"humanApproval,omitempty"`

	// GovernedResourceRef records which GovernedResource admitted this AgentRequest.
	// Set by the gateway at admission time. Immutable after creation.
	// Empty only when --require-governed-resource=false and no GovernedResources exist.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="governedResourceRef is immutable after creation"
	GovernedResourceRef *GovernedResourceRef `json:"governedResourceRef,omitempty"`
}

// GovernedResourceRef records which GovernedResource admitted this AgentRequest.
type GovernedResourceRef struct {
	Name       string `json:"name"`
	Generation int64  `json:"generation"`
}

// HumanApproval captures a human reviewer's explicit decision on an AgentRequest.
type HumanApproval struct {
	// Decision is the reviewer's choice: "approved" or "denied".
	// +kubebuilder:validation:Enum=approved;denied
	Decision string `json:"decision"`
	// Reason is an optional free-text justification for the decision.
	// +optional
	Reason string `json:"reason,omitempty"`
	// ForGeneration binds this approval to a specific EvaluationGeneration epoch.
	ForGeneration int64 `json:"forGeneration"`
}

type Target struct {
	// +kubebuilder:validation:MinLength=1
	URI          string            `json:"uri"` // e.g., k8s://prod/default/deployment/payment-api
	ResourceType *string           `json:"resourceType,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

type CascadeModel struct {
	// +kubebuilder:validation:MinItems=1
	AffectedTargets []AffectedTarget `json:"affectedTargets"`
	// +kubebuilder:validation:Enum=authoritative;derived;inferred
	ModelSourceTrust *string `json:"modelSourceTrust,omitempty"`
	ModelSourceID    *string `json:"modelSourceID,omitempty"` // e.g., "cmdb-v2.3", "topology-api/2024-01"
}

type AffectedTarget struct {
	// +kubebuilder:validation:MinLength=1
	URI string `json:"uri"`
	// +kubebuilder:validation:Enum=deleted;modified;disrupted;orphaned
	EffectType string `json:"effectType"`
}

type ReasoningTrace struct {
	// +kubebuilder:validation:Type=number
	// +kubebuilder:validation:Format=float
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	ConfidenceScore     *float64           `json:"confidenceScore,omitempty"`     // 0.0–1.0
	ComponentConfidence map[string]float64 `json:"componentConfidence,omitempty"` // e.g., {"diagnosis": 0.95, "remediation_selection": 0.70}
	CalibrationEvidence *string            `json:"calibrationEvidence,omitempty"` // Signed performance metrics reference
	TraceReference      *string            `json:"traceReference,omitempty"`      // Link to chain-of-thought log
	Alternatives        []string           `json:"alternatives,omitempty"`
}

// ScopeBounds defines the operating envelope for an agent using executionMode: scoped.
// The control plane approves the envelope at scope-request time and acquires OpsLocks on
// all resources currently matching PermittedTargetPatterns (spec Section 3.3).
type ScopeBounds struct {
	// PermittedActions lists the action types the agent may perform within this scope.
	// +kubebuilder:validation:MinItems=1
	PermittedActions []string `json:"permittedActions"`
	// PermittedTargetPatterns lists URI glob patterns the agent may operate on.
	// OpsLocks are acquired on all currently-matching resources at approval time.
	// +kubebuilder:validation:MinItems=1
	PermittedTargetPatterns []string `json:"permittedTargetPatterns"`
	// TimeBoundSeconds is the maximum duration for the scoped operation.
	// The control plane MUST revoke the scope after this period.
	// +kubebuilder:validation:Minimum=1
	TimeBoundSeconds int32 `json:"timeBoundSeconds"`
}

// Phases (computed from Conditions)
const (
	PhasePending   = "Pending"
	PhaseApproved  = "Approved"
	PhaseDenied    = "Denied"
	PhaseExecuting = "Executing"
	PhaseCompleted = "Completed"
	PhaseFailed    = "Failed"
)

// Condition types
const (
	ConditionPolicyEvaluated  = "PolicyEvaluated"  // True when all policies evaluated successfully
	ConditionApproved         = "Approved"         // True when approved, False when denied
	ConditionLockAcquired     = "LockAcquired"     // True when OpsLock held
	ConditionRequiresApproval = "RequiresApproval" // True when held for external human approval
	ConditionExecuting        = "Executing"        // True when agent is actively executing
	ConditionCompleted        = "Completed"        // True on success
	ConditionFailed           = "Failed"           // True on failure
)

// Denial error codes (from AIP spec Section 3.1.1)
const (
	DenialCodePolicyViolation         = "POLICY_VIOLATION"
	DenialCodeLockContention          = "LOCK_CONTENTION"
	DenialCodeLockTimeout             = "LOCK_TIMEOUT"
	DenialCodeRateLimited             = "RATE_LIMITED"
	DenialCodeEvaluationFailure       = "EVALUATION_FAILURE"
	DenialCodeIdentityInvalid         = "IDENTITY_INVALID"
	DenialCodePlanAborted             = "PLAN_ABORTED"
	DenialCodeActionNotPermitted      = "ACTION_NOT_PERMITTED"
	DenialCodeCascadeDenied           = "CASCADE_DENIED"
	DenialCodeApprovalRevoked         = "APPROVAL_REVOKED"
	DenialCodeStateDrifted            = "STATE_DRIFTED"
	DenialCodeGenerationMismatch      = "GENERATION_MISMATCH"
	DenialCodeScopeTooBoard           = "SCOPE_TOO_BROAD"
	DenialCodeGovernedResourceDeleted = "GOVERNED_RESOURCE_DELETED"
)

type AgentRequestStatus struct {
	Phase      string             `json:"phase,omitempty"` // Computed summary
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	Denial     *DenialResponse    `json:"denial,omitempty"`
	LeaseName  string             `json:"leaseName,omitempty"` // Name of the K8s Lease

	// EvaluationGeneration is incremented each time the control plane performs
	// a fresh policy evaluation on the request.
	EvaluationGeneration int64 `json:"evaluationGeneration,omitempty"`

	// ControlPlaneVerification holds live cluster state independently fetched by
	// the AIP control plane before policy evaluation. This is persisted so that
	// human reviewers in the dashboard can see what AIP verified vs what the
	// agent declared — the two sides of the governance decision.
	ControlPlaneVerification *ControlPlaneVerification `json:"controlPlaneVerification,omitempty"`

	// ProviderContext holds live resource state fetched by the context fetcher
	// named in the matching GovernedResource. Schema is fetcher-specific.
	// +optional
	ProviderContext *apiextensionsv1.JSON `json:"providerContext,omitempty"`
}

// ControlPlaneVerification captures live cluster state that the AIP control
// plane fetched independently of the agent's intent declaration.
type ControlPlaneVerification struct {
	// EvaluatedStateFingerprint is the opaque fingerprint of the target resource's
	// state at the time of policy evaluation (T1).
	EvaluatedStateFingerprint string `json:"evaluatedStateFingerprint,omitempty"`
	// TargetExists is true when the target resource was found in the cluster.
	TargetExists bool `json:"targetExists"`
	// HasActiveEndpoints is true when the target service has ready endpoints.
	HasActiveEndpoints bool `json:"hasActiveEndpoints"`
	// ActiveEndpointCount is the number of ready endpoint addresses.
	ActiveEndpointCount int `json:"activeEndpointCount,omitempty"`
	// ReadyReplicas is the number of ready replicas observed on the target deployment.
	ReadyReplicas int `json:"readyReplicas,omitempty"`
	// SpecReplicas is the desired replica count in the deployment spec.
	SpecReplicas int `json:"specReplicas,omitempty"`
	// DownstreamServices lists services the control plane found depending on this target.
	DownstreamServices []string `json:"downstreamServices,omitempty"`
	// FetchedAt is when the control plane fetched this data.
	FetchedAt metav1.Time `json:"fetchedAt"`
}

type DenialResponse struct {
	Code              string         `json:"code"` // One of the DenialCode* constants
	Message           string         `json:"message"`
	PolicyResults     []PolicyResult `json:"policyResults,omitempty"`
	RetryAfterSeconds *int32         `json:"retryAfterSeconds,omitempty"`
}

// Policy evaluation results
const (
	ResultAllow            = "Allow"
	ResultDeny             = "Deny"
	ResultLog              = "Log"
	ResultRequireApproval  = "RequireApproval"
	ResultEvaluationFailed = "EvaluationFailed"
)

type PolicyResult struct {
	PolicyName string `json:"policyName"`
	RuleName   string `json:"ruleName"`
	Result     string `json:"result"` // Use Result* constants

	// PolicyGeneration records the generation of the policy at the time of evaluation.
	// +optional
	PolicyGeneration int64 `json:"policyGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentIdentity`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.uri`,priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AgentRequest is the Schema for the agentrequests API
type AgentRequest struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of AgentRequest
	// +required
	Spec AgentRequestSpec `json:"spec"`

	// status defines the observed state of AgentRequest
	// +optional
	Status AgentRequestStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AgentRequestList contains a list of AgentRequest
type AgentRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AgentRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentRequest{}, &AgentRequestList{})
}
