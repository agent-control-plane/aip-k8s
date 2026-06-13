package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

// agentRequestPhaseIndexKey is the field index key for status.phase on AgentRequest.
// Used in both the manager IndexField registration and the MatchingFields list call.
const agentRequestPhaseIndexKey = "status.phase"

// agentRequestPhaseIndexFunc is the shared indexer function used by both the
// manager's IndexField registration (main.go) and the fake client in tests.
// Keeping a single definition ensures they index on the same field.
func agentRequestPhaseIndexFunc(obj client.Object) []string {
	ar, ok := obj.(*v1alpha1.AgentRequest)
	if !ok || ar.Status.Phase == "" {
		return nil
	}
	return []string{ar.Status.Phase}
}

// validPhases is the set of known AgentRequest phases for client-side filtering.
// Kept in sync with the constants in api/v1alpha1/agentrequest_types.go.
var validPhases = map[string]bool{
	v1alpha1.PhasePending:         true,
	v1alpha1.PhaseApproved:        true,
	v1alpha1.PhaseDenied:          true,
	v1alpha1.PhaseExecuting:       true,
	v1alpha1.PhaseCompleted:       true,
	v1alpha1.PhaseFailed:          true,
	v1alpha1.PhaseAwaitingVerdict: true,
	v1alpha1.PhaseExpired:         true,
	v1alpha1.PhaseObserved:        true,
}

// computeDedupKey returns the dedup key for an AgentRequest submission.
// If the agent supplied an explicit dedupKey it is used verbatim.
// Otherwise the key is derived from (agentIdentity, action, targetURI, classification)
// using null-byte separators to prevent cross-field collisions.
// classification must already be normalised by the caller (normalizeClassification).
func computeDedupKey(agentIdentity, action, targetURI, classification, explicit string) string {
	if explicit != "" {
		return explicit
	}
	h := sha256.New()
	for _, s := range []string{agentIdentity, action, targetURI, classification} {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// deterministicRequestName returns a stable K8s object name for an AgentRequest
// submission. Within one dedupWindow the same (dedupKey, windowIndex) produces the
// same name, so a K8s Create returns AlreadyExists for true duplicates (atomic —
// no List→Create race). A new window produces a new name, allowing recurrence.
//
// Name format: <agent-slug>-<8 hex chars>
// Total length is bounded to ≤ 63 chars (K8s name limit for most resources).
func deterministicRequestName(agentIdentity, dedupKey string, dedupWindow time.Duration, now time.Time) string {
	windowIndex := now.UnixNano() / int64(dedupWindow)
	h := sha256.New()
	_, _ = h.Write([]byte(dedupKey))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strconv.FormatInt(windowIndex, 10)))
	hashSuffix := hex.EncodeToString(h.Sum(nil))[:8]
	slug := sanitizeDNSSegment(agentIdentity, 54)
	return slug + "-" + hashSuffix
}

//nolint:gocyclo // handler covers full admission pipeline: auth, dedup, GR match, SoakMode, create, poll
func (s *Server) handleCreateAgentRequest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}

	var body createAgentRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Action == "" || body.TargetURI == "" {
		writeError(w, http.StatusBadRequest, "action and targetURI are required")
		return
	}

	var reg *v1alpha1.AgentRegistration
	if s.regCache != nil && body.AgentIdentity != "" {
		reg = s.regCache.get(body.AgentIdentity)
	}

	// 1. Role check.
	if s.authRequired {
		if !requireRole(s.roles, roleAgent, sub, callerGroupsFromCtx(r.Context()), w) {
			return
		}
	}

	var agentIdentity string
	if reg != nil {
		agentIdentity = reg.Spec.AgentIdentity
	} else {
		if s.authRequired {
			agentIdentity = sub
		} else {
			if body.AgentIdentity == "" {
				writeError(w, http.StatusBadRequest, "agentIdentity is required when running without authentication")
				return
			}
			agentIdentity = body.AgentIdentity
		}
	}

	ns := body.Namespace
	if ns == "" {
		ns = defaultNamespace
	}

	var parameters *apiextensionsv1.JSON
	if len(body.Parameters) > 0 && string(body.Parameters) != "null" {
		parameters = &apiextensionsv1.JSON{Raw: body.Parameters}
	}

	reqLabels := map[string]string{
		"aip.io/agentIdentity": sanitizeLabelValue(agentIdentity),
		"aip.io/profileName":   v1alpha1.ProfileNameForAgent(agentIdentity),
	}
	if body.CorrelationID != "" {
		reqLabels["aip.io/correlationID"] = sanitizeLabelValue(body.CorrelationID)
	}
	if body.Classification != "" {
		reqLabels["aip.io/classification"] = sanitizeLabelValue(body.Classification)
	}

	agentReq := &v1alpha1.AgentRequest{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", sanitizeDNSSegment(agentIdentity, 57)),
			Namespace:    ns,
			Labels:       reqLabels,
		},
		Spec: v1alpha1.AgentRequestSpec{
			AgentIdentity:  agentIdentity,
			Action:         body.Action,
			Target:         v1alpha1.Target{URI: body.TargetURI},
			Reason:         body.Reason,
			Classification: normalizeClassification(body.Classification),
			CascadeModel:   buildCascadeModel(&body),
			ReasoningTrace: buildReasoningTrace(&body),
			Parameters:     parameters,
			ExecutionMode:  body.ExecutionMode,
			ScopeBounds:    body.ScopeBounds,
		},
	}

	if s.regCache != nil {
		if reg == nil {
			switch s.unregisteredAgentPolicy {
			case "strict":
				writeError(w, http.StatusForbidden,
					fmt.Sprintf("AGENT_NOT_REGISTERED: agent %q has no AgentRegistration", body.AgentIdentity))
				return
			case "warn":
				log.Printf("Unregistered AgentRequest submitted agentIdentity=%q policy=%q",
					body.AgentIdentity, s.unregisteredAgentPolicy)
				if agentReq.Annotations == nil {
					agentReq.Annotations = map[string]string{}
				}
				agentReq.Annotations["governance.aip.io/unregistered"] = "true"
			}
			// "allow": proceed silently — backward-compatible default
		} else {
			issuer := callerIssuerFromCtx(r.Context())
			if err := validateOIDCIdentity(reg, issuer, sub); err != nil {
				writeError(w, http.StatusForbidden, fmt.Sprintf("IDENTITY_MISMATCH: %v", err))
				return
			}
		}
	}

	// GovernedResource admission: URI → agent identity → action (per design doc order).
	var matchedGR *v1alpha1.GovernedResource
	var govResources v1alpha1.GovernedResourceList
	var agentPermitted, actionPermitted bool
	if err := s.client.List(r.Context(), &govResources); err != nil {
		// If the CRD is not yet installed, treat as an empty list.
		// This allows the system to boot gracefully even if the GovernedResource CRD
		// is not yet available (e.g., during cluster initialization in e2e tests).
		if meta.IsNoMatchError(err) {
			// CRD not yet installed — treat as empty list
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to list GovernedResources: %v", err))
			return
		}
	}

	// Backward compat: skip check when no GovernedResources exist and flag is false.
	if len(govResources.Items) == 0 && !s.requireGovernedResource {
		goto admissionPassed
	}

	// URI-only match here — the action check below (with its audit-trail path) is
	// the authoritative gate for the HTTP handler. Passing action="" keeps the
	// most-specific-URI-wins semantics intact across multiple GovernedResources:
	// a more-specific GR that restricts actions must not be bypassed by falling
	// back to a less-specific GR that happens to permit the action.
	matchedGR = matchGovernedResource(govResources.Items, body.TargetURI, "")
	if matchedGR == nil {
		writeError(w, http.StatusForbidden, v1alpha1.DenialCodeActionNotPermitted)
		return
	}

	// Check agent identity (step 3 in design doc).
	if len(matchedGR.Spec.PermittedAgents) > 0 {
		agentPermitted = slices.Contains(matchedGR.Spec.PermittedAgents, agentIdentity)
		if !agentPermitted {
			writeError(w, http.StatusForbidden, v1alpha1.DenialCodeIdentityInvalid)
			return
		}
	}

	// Check action (step 4 in design doc).
	actionPermitted = slices.Contains(matchedGR.Spec.PermittedActions, body.Action)
	if !actionPermitted {
		// Create an AgentRequest with the gateway-denied annotation so the controller
		// emits a request.denied AuditRecord — scope escalation attempts must be visible
		// in the audit trail even though no K8s resource would otherwise be created.
		agentReq.Spec.GovernedResourceRef = &v1alpha1.GovernedResourceRef{
			Name:       matchedGR.Name,
			Generation: matchedGR.Generation,
		}
		if agentReq.Annotations == nil {
			agentReq.Annotations = make(map[string]string)
		}
		agentReq.Annotations[v1alpha1.AnnotationGatewayDenied] = v1alpha1.DenialCodeActionNotPermitted
		createErr := s.client.Create(r.Context(), agentReq)
		if createErr == nil {
			patchBase := agentReq.DeepCopy()
			agentReq.Status.GovernedResourceRef = &v1alpha1.GovernedResourceRef{
				Name:       matchedGR.Name,
				Generation: matchedGR.Generation,
			}
			if patchErr := s.client.Status().Patch(r.Context(), agentReq, client.MergeFrom(patchBase)); patchErr != nil {
				log.Printf("Failed to patch GovernedResourceRef to status for denied request: %v", patchErr)
			}
		} else {
			log.Printf("failed to create gateway-denied AgentRequest for audit trail: %v", createErr)
		}
		writeError(w, http.StatusForbidden, v1alpha1.DenialCodeActionNotPermitted)
		return
	}

admissionPassed:
	if matchedGR != nil {
		agentReq.Spec.GovernedResourceRef = &v1alpha1.GovernedResourceRef{
			Name:       matchedGR.Name,
			Generation: matchedGR.Generation,
		}
		// SoakMode phase initialization is handled exclusively by the controller.
		// The gateway sets GovernedResourceRef so the controller can detect SoakMode
		// on its first reconcile and route to PhaseAwaitingVerdict.
	}

	// Trust gate: enforce trust level requirements from GovernedResource.
	if matchedGR != nil && matchedGR.Spec.TrustRequirements != nil {
		trustResult, err := s.evaluateTrustGate(r.Context(), ns, agentIdentity, agentReq.Spec.Mode, matchedGR)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("trust gate error: %v", err))
			return
		}
		if trustResult.rejected {
			writeError(w, http.StatusForbidden, fmt.Sprintf("INSUFFICIENT_TRUST: %s", trustResult.message))
			return
		}
		if trustResult.annotations != nil {
			if agentReq.Annotations == nil {
				agentReq.Annotations = make(map[string]string)
			}
			maps.Copy(agentReq.Annotations, trustResult.annotations)
		}
	}

	// Deterministic naming for dedup (when dedupWindow > 0).
	// Replace GenerateName with a stable name so K8s Create is the atomic dedup check —
	// AlreadyExists means a live duplicate; a new window bucket allows recurrence.
	// agentReq.Spec.Classification is already normalised (set above); reuse it here
	// rather than calling normalizeClassification a second time.
	if s.dedupWindow > 0 {
		clk := s.Clock
		if clk == nil {
			clk = time.Now
		}
		dedupKey := computeDedupKey(
			agentIdentity, body.Action, body.TargetURI,
			agentReq.Spec.Classification, body.DedupKey,
		)
		agentReq.Name = deterministicRequestName(agentIdentity, dedupKey, s.dedupWindow, clk())
		agentReq.GenerateName = ""
		// Only persist the dedupKey field when the agent explicitly provided one.
		// When the key is gateway-computed, the field stays absent so operators
		// can distinguish "agent-set" from "auto-computed" post-creation.
		if body.DedupKey != "" {
			agentReq.Spec.DedupKey = body.DedupKey
		}
	}

	if err := s.client.Create(r.Context(), agentReq); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Duplicate within the dedup window: fetch the existing request.
			var existing v1alpha1.AgentRequest
			nn := types.NamespacedName{Name: agentReq.Name, Namespace: ns}
			if getErr := s.client.Get(r.Context(), nn, &existing); getErr != nil {
				writeError(w, http.StatusInternalServerError,
					fmt.Sprintf("duplicate detected but failed to fetch existing: %v", getErr))
				return
			}
			// If the existing request is in a terminal phase (Completed/Failed/Denied/
			// Expired) it is a stale object from a prior window that GC has not yet
			// removed. Delete it so the next window produces a fresh request.
			// Return 409 so the agent retries immediately — the stale object is now
			// gone and the retry will succeed.
			if terminalPhases[existing.Status.Phase] {
				if delErr := s.client.Delete(r.Context(), &existing); delErr != nil && !apierrors.IsNotFound(delErr) {
					writeError(w, http.StatusInternalServerError,
						fmt.Sprintf("dedup: failed to delete stale terminal request %s: %v", existing.Name, delErr))
					return
				}
				writeError(w, http.StatusConflict,
					"previous request for this key is terminal (Completed/Failed/Denied/Expired) — stale object deleted, please retry")
				return
			}
			// Active duplicate: return the existing request as HTTP 200.
			payload := map[string]any{
				"name":                     existing.Name,
				"labels":                   reqLabels,
				"phase":                    existing.Status.Phase,
				"denial":                   existing.Status.Denial,
				"conditions":               existing.Status.Conditions,
				"controlPlaneVerification": existing.Status.ControlPlaneVerification,
			}
			if acceptsSSE(r) {
				rc := http.NewResponseController(w)
				writeSSEHeaders(w)
				if err := rc.Flush(); err != nil {
					log.Printf("SSE: failed to flush for duplicate %s: %v", existing.Name, err)
					return
				}
				if err := writeSSEEvent(w, rc, sseEventResult, payload); err != nil {
					log.Printf("SSE: failed to write duplicate result for %s: %v", existing.Name, err)
				}
				return
			}
			writeJSON(w, http.StatusOK, payload)
			return
		}
		if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid AgentRequest: %v", err))
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create AgentRequest: %v", err))
		return
	}

	// Mirror GovernedResourceRef to Status for the controller to consume.
	// This is done after creation since Status is a subresource.
	if matchedGR != nil {
		patchBase := agentReq.DeepCopy()
		agentReq.Status.GovernedResourceRef = &v1alpha1.GovernedResourceRef{
			Name:       matchedGR.Name,
			Generation: matchedGR.Generation,
		}
		if patchErr := s.client.Status().Patch(r.Context(), agentReq, client.MergeFrom(patchBase)); patchErr != nil {
			log.Printf("Failed to patch GovernedResourceRef to status: %v", patchErr)
		}
	}

	if acceptsSSE(r) {
		s.streamAgentRequestPhase(w, r, agentReq.Name, ns, reqLabels)
	} else {
		s.pollAgentRequestPhase(w, r, agentReq.Name, ns, reqLabels)
	}
}

func (s *Server) pollAgentRequestPhase(
	w http.ResponseWriter,
	r *http.Request,
	name, ns string,
	reqLabels map[string]string,
) {
	ctx, cancel := context.WithTimeout(r.Context(), s.waitTimeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if r.Context().Err() == nil {
				// r.Context() is still live — our waitTimeout fired; write 504.
				writeError(w, http.StatusGatewayTimeout, "timed out waiting for AgentRequest resolution")
			}
			// r.Context() is done: client disconnected — can't write a response.
			return
		case <-ticker.C:
			var current v1alpha1.AgentRequest
			if err := s.client.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &current); err != nil {
				continue
			}

			phase := current.Status.Phase
			if phase == v1alpha1.PhaseApproved || phase == v1alpha1.PhaseDenied ||
				phase == v1alpha1.PhaseCompleted || phase == v1alpha1.PhaseFailed ||
				phase == v1alpha1.PhaseAwaitingVerdict {
				writeJSON(w, http.StatusCreated, map[string]any{
					"name":                     current.Name,
					"labels":                   reqLabels,
					"phase":                    current.Status.Phase,
					"denial":                   current.Status.Denial,
					"conditions":               current.Status.Conditions,
					"controlPlaneVerification": current.Status.ControlPlaneVerification,
					"result":                   current.Status.Result,
				})
				return
			}

			// Return early when human approval is required — the agent
			// should not block waiting for a human decision.
			if phase == v1alpha1.PhasePending &&
				meta.IsStatusConditionTrue(current.Status.Conditions, v1alpha1.ConditionRequiresApproval) {
				writeJSON(w, http.StatusCreated, map[string]any{
					"name":                     current.Name,
					"labels":                   reqLabels,
					"phase":                    current.Status.Phase,
					"conditions":               current.Status.Conditions,
					"controlPlaneVerification": current.Status.ControlPlaneVerification,
				})
				return
			}
		}
	}
}
