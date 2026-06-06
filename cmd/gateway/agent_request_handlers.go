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
	"strings"
	"time"
	"unicode/utf8"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
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

	var agentIdentity string
	var unregistered bool
	var reg *v1alpha1.AgentRegistration
	if s.regCache != nil {
		reg = s.regCache.getForSubject(body.AgentIdentity, sub)
	}

	if reg != nil {
		agentIdentity = reg.Spec.AgentIdentity
		// Validate OIDC allowedSubjects if configured in the registration
		if s.authRequired && reg.Spec.OIDC != nil && len(reg.Spec.OIDC.AllowedSubjects) > 0 {
			if err := validateOIDCSubject(reg, sub); err != nil {
				writeError(w, http.StatusForbidden, "IDENTITY_MISMATCH: "+err.Error())
				return
			}
		} else {
			// Fallback to global role check if registration doesn't specify AllowedSubjects
			if !requireRole(s.roles, roleAgent, sub, callerGroupsFromCtx(r.Context()), w) {
				return
			}
		}
	} else {
		// getForSubject returned nil: either no registration exists for this agent, or one
		// exists but the caller's OIDC subject is not in its AllowedSubjects list.
		if s.regCache != nil && body.AgentIdentity != "" && s.regCache.exists(body.AgentIdentity) {
			writeError(w, http.StatusForbidden, "IDENTITY_MISMATCH: OIDC subject not in allowedSubjects")
			return
		}

		if !requireRole(s.roles, roleAgent, sub, callerGroupsFromCtx(r.Context()), w) {
			return
		}

		if s.regCache != nil {
			unregistered = true
			switch s.unregisteredAgentPolicy {
			case policyStrict:
				writeError(w, http.StatusForbidden, "AGENT_NOT_REGISTERED")
				return
			case policyWarn:
				log.Printf("Unregistered agent, policy=warn, agentIdentity=%q", body.AgentIdentity)
			}
		}

		// Resolve default/open identity
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

	if unregistered && s.unregisteredAgentPolicy == policyWarn {
		if agentReq.Annotations == nil {
			agentReq.Annotations = make(map[string]string)
		}
		agentReq.Annotations["governance.aip.io/unregistered"] = annotationValueTrue
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

func (s *Server) handleGetAgentRequest(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}

	name := r.PathValue("name")
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var current v1alpha1.AgentRequest
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &current); err != nil {
		writeError(w, http.StatusNotFound, "AgentRequest not found")
		return
	}

	var auditList v1alpha1.AuditRecordList
	if err := s.client.List(r.Context(), &auditList, client.InNamespace(ns)); err != nil {
		log.Printf("failed to list AuditRecords: %v", err)
		// continue regardless, just return empty list
	}

	auditEvents := []string{}
	for _, a := range auditList.Items {
		if a.Spec.AgentRequestRef == name {
			auditEvents = append(auditEvents, a.Spec.Event)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":                     current.Name,
		"phase":                    current.Status.Phase,
		"denial":                   current.Status.Denial,
		"conditions":               current.Status.Conditions,
		"controlPlaneVerification": current.Status.ControlPlaneVerification,
		"auditEvents":              auditEvents,
		"result":                   current.Status.Result,
	})
}

//nolint:dupl // structurally similar to handleCompletedAgentRequest
func (s *Server) handleExecutingAgentRequest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	if !requireRole(s.roles, roleAgent, sub, callerGroupsFromCtx(r.Context()), w) {
		return
	}

	name := r.PathValue("name")
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var req v1alpha1.AgentRequest
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &req); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AgentRequest not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	if s.authRequired && req.Spec.AgentIdentity != sub {
		writeError(w, http.StatusForbidden, "forbidden: only the creating agent may transition this request")
		return
	}

	if req.Status.Phase != v1alpha1.PhaseApproved {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("request is in phase %q — can only transition to Executing from Approved", req.Status.Phase))
		return
	}

	s.patchAgentRequestCondition(w, r, v1alpha1.ConditionExecuting, "AgentStarted", "Agent is now executing action")
}

//nolint:dupl // structurally similar to handleExecutingAgentRequest
func (s *Server) handleCompletedAgentRequest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	if !requireRole(s.roles, roleAgent, sub, callerGroupsFromCtx(r.Context()), w) {
		return
	}

	name := r.PathValue("name")
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var req v1alpha1.AgentRequest
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &req); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AgentRequest not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	if s.authRequired && req.Spec.AgentIdentity != sub {
		writeError(w, http.StatusForbidden, "forbidden: only the creating agent may transition this request")
		return
	}

	if req.Status.Phase != v1alpha1.PhaseExecuting {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("request is in phase %q — can only transition to Completed from Executing", req.Status.Phase))
		return
	}

	s.patchAgentRequestCondition(w, r, v1alpha1.ConditionCompleted,
		"ActionSuccess", "Agent successfully completed the action")
}

//nolint:dupl // structurally similar to handleCompletedAgentRequest
func (s *Server) handlePutAgentRequestResult(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	if !requireRole(s.roles, roleAgent, sub, callerGroupsFromCtx(r.Context()), w) {
		return
	}

	// Validate input before any K8s API call so malformed requests are rejected cheaply.
	var body struct {
		URL     string `json:"url"`
		Summary string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.URL == "" || !strings.HasPrefix(body.URL, "https://") || len(body.URL) < 9 {
		writeError(w, http.StatusBadRequest, "url must be a valid https URL")
		return
	}
	if utf8.RuneCountInString(body.Summary) > 512 {
		writeError(w, http.StatusBadRequest, "summary must be at most 512 characters")
		return
	}

	name := r.PathValue("name")
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var req v1alpha1.AgentRequest
	if err := s.apiReader.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &req); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AgentRequest not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	// Only the creating agent can record a result.
	if s.authRequired && req.Spec.AgentIdentity != sub {
		writeError(w, http.StatusForbidden, "forbidden: only the creating agent may record a result")
		return
	}

	var wrongPhase string
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current v1alpha1.AgentRequest
		if err := s.apiReader.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &current); err != nil {
			return err
		}

		// Allow recording a result on Executing OR Completed. Accepting Completed
		// closes the ordering race: if the agent calls /completed before /result
		// (accidentally or under a crash-restart), the result is not silently lost.
		// All other phases are terminal-without-result or pre-execution — reject them.
		if current.Status.Phase != v1alpha1.PhaseExecuting &&
			current.Status.Phase != v1alpha1.PhaseCompleted {
			wrongPhase = current.Status.Phase
			return fmt.Errorf("wrong phase") // not a 409 conflict; RetryOnConflict will not retry this
		}

		base := current.DeepCopy()
		current.Status.Result = &v1alpha1.AgentRequestResult{
			URL:     body.URL,
			Summary: body.Summary,
		}
		return s.client.Status().Patch(r.Context(), &current, client.MergeFrom(base))
	}); err != nil {
		if wrongPhase != "" {
			writeError(w, http.StatusConflict,
				fmt.Sprintf("request is in phase %q — result can only be recorded in Executing or Completed", wrongPhase))
			return
		}
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AgentRequest not found")
		} else {
			writeError(w, http.StatusInternalServerError,
				fmt.Sprintf("failed to record result: %v", err))
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "result recorded"})
}

func (s *Server) patchAgentRequestCondition(
	w http.ResponseWriter, r *http.Request, conditionType, reason, message string,
) {
	name := r.PathValue("name")
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current v1alpha1.AgentRequest
		if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &current); err != nil {
			return err
		}

		base := current.DeepCopy()
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionTrue,
			Reason:  reason,
			Message: message,
		})

		return s.client.Status().Patch(r.Context(), &current, client.MergeFrom(base))
	}); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AgentRequest not found")
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to update status: %v", err))
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"message": fmt.Sprintf("successfully patched condition %s", conditionType),
	})
}

func (s *Server) handleListAgentRequests(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}

	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	agentID := r.URL.Query().Get("agentIdentity")
	correlID := r.URL.Query().Get("correlationID")
	classification := r.URL.Query().Get("classification")
	phaseParam := r.URL.Query().Get("phase")
	limitStr := r.URL.Query().Get("limit")
	continueToken := r.URL.Query().Get("continue")

	var requestedPhases []string
	if phaseParam != "" {
		for p := range strings.SplitSeq(phaseParam, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if !validPhases[p] {
				valid := make([]string, 0, len(validPhases))
				for ph := range validPhases {
					valid = append(valid, ph)
				}
				slices.Sort(valid)
				writeError(w, http.StatusBadRequest,
					fmt.Sprintf("invalid phase %q: must be one of %s", p, strings.Join(valid, ", ")))
				return
			}
			requestedPhases = append(requestedPhases, p)
		}
	}

	listOpts := []client.ListOption{client.InNamespace(ns)}
	matchLabels := map[string]string{}
	if agentID != "" {
		matchLabels["aip.io/agentIdentity"] = sanitizeLabelValue(agentID)
	}
	if correlID != "" {
		matchLabels["aip.io/correlationID"] = sanitizeLabelValue(correlID)
	}
	if classification != "" {
		matchLabels["aip.io/classification"] = sanitizeLabelValue(classification)
	}
	if len(matchLabels) > 0 {
		listOpts = append(listOpts, client.MatchingLabels(matchLabels))
	}
	// Single-phase: use field indexer — server-side, pagination-correct.
	if len(requestedPhases) == 1 {
		listOpts = append(listOpts, client.MatchingFields{agentRequestPhaseIndexKey: requestedPhases[0]})
	}
	if limitStr != "" {
		limit, err := strconv.ParseInt(limitStr, 10, 64)
		if err != nil || limit <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit: must be a positive integer")
			return
		}
		listOpts = append(listOpts, client.Limit(limit))
	}
	if continueToken != "" {
		listOpts = append(listOpts, client.Continue(continueToken))
	}

	var list v1alpha1.AgentRequestList
	if err := s.client.List(r.Context(), &list, listOpts...); err != nil {
		if apierrors.IsBadRequest(err) || apierrors.IsInvalid(err) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	items := list.Items
	if items == nil {
		items = []v1alpha1.AgentRequest{}
	}

	// Multi-phase (>1) fallback: client-side filter after the K8s List.
	// Single-phase uses the field indexer (server-side) above.
	if len(requestedPhases) > 1 {
		filtered := make([]v1alpha1.AgentRequest, 0, len(items))
		for _, item := range items {
			if slices.Contains(requestedPhases, item.Status.Phase) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}

	if limitStr != "" || continueToken != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"items":    items,
			"continue": list.Continue,
		})
	} else {
		writeJSON(w, http.StatusOK, items)
	}
}

func validateOIDCSubject(reg *v1alpha1.AgentRegistration, sub string) error {
	if reg.Spec.OIDC == nil || len(reg.Spec.OIDC.AllowedSubjects) == 0 {
		return nil
	}
	if slices.Contains(reg.Spec.OIDC.AllowedSubjects, sub) {
		return nil
	}
	return fmt.Errorf("token subject %q not in allowedSubjects for agent %q",
		sub, reg.Spec.AgentIdentity)
}
