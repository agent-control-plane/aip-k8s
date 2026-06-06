package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/http"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/mcp"
)

// aipToolDefs are the AIP governance tools exposed via the MCP interface.
var aipToolDefs = []mcp.MCPToolInfo{
	{
		Name: "aip/await_approval",
		Description: "Wait for an AIP governance request to be approved or denied. " +
			"Returns the AIP JWT on approval for use as _aip_authorization in the subsequent tool call.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"requestId": map[string]string{
					"type":        "string",
					"description": "The AgentRequest ID returned in the pending_approval response",
				},
			},
			"required": []string{"requestId"},
		},
	},
}

// handleAIPTool dispatches aip/* tool calls to their internal handlers.
func (s *Server) handleAIPTool(
	w http.ResponseWriter, r *http.Request, req *mcp.JSONRPCRequest, toolName string, args map[string]any,
) {
	switch toolName {
	case "await_approval":
		s.handleAIPAwaitApproval(w, r, req, args)
	default:
		if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInvalid, "unknown aip tool: "+toolName); wErr != nil {
			log.Printf("WriteJSONRPCError failed: %v", wErr)
		}
	}
}

// handleAIPAwaitApproval blocks until the AgentRequest reaches a terminal phase,
// then returns the result: JWT on approval, denial reason on denial.
func (s *Server) handleAIPAwaitApproval(
	w http.ResponseWriter, r *http.Request, req *mcp.JSONRPCRequest, args map[string]any,
) {
	requestID, _ := args["requestId"].(string)
	if requestID == "" {
		if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInvalid, "requestId is required"); wErr != nil {
			log.Printf("WriteJSONRPCError failed: %v", wErr)
		}
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.waitTimeout)
	defer cancel()

	// Use apiReader (direct API server read) to avoid informer-cache lag: the
	// AgentRequest may have been created milliseconds ago by governanceSubmissionPath
	// and the cached client may not have received the watch event yet.
	var ar v1alpha1.AgentRequest
	if err := s.apiReader.Get(ctx, client.ObjectKey{Namespace: defaultNamespace, Name: requestID}, &ar); err != nil {
		msg := "AgentRequest not found: " + requestID
		if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInvalid, msg); wErr != nil {
			log.Printf("WriteJSONRPCError failed: %v", wErr)
		}
		return
	}

	if result := s.aipApprovalResult(&ar); result != nil {
		if wErr := mcp.WriteJSONRPCResponse(w, req.ID, *result); wErr != nil {
			log.Printf("WriteJSONRPCResponse failed: %v", wErr)
		}
		return
	}

	watcher, err := s.watchClient.Watch(ctx, &v1alpha1.AgentRequestList{},
		client.InNamespace(defaultNamespace),
		client.MatchingFields{"metadata.name": requestID},
		&client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: ar.ResourceVersion}},
	)
	if err != nil {
		if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInternal,
			"failed to watch request: "+err.Error()); wErr != nil {
			log.Printf("WriteJSONRPCError failed: %v", wErr)
		}
		return
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			// Release the lock: fail any Pending/Approved request so the
			// controller can clean up. Use a fresh context since r.Context() is done.
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), mcpRequestTimeout)
			defer cleanupCancel()
			s.failAgentRequest(cleanupCtx, requestID, "timed out waiting for agent to complete execution")
			if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInternal, "timed out waiting for approval"); wErr != nil {
				log.Printf("WriteJSONRPCError failed: %v", wErr)
			}
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInternal, "watch channel closed unexpectedly"); wErr != nil {
					log.Printf("WriteJSONRPCError failed: %v", wErr)
				}
				return
			}
			if event.Type == watch.Error || event.Type == watch.Deleted {
				if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInternal, "request watch error or deleted"); wErr != nil {
					log.Printf("WriteJSONRPCError failed: %v", wErr)
				}
				return
			}
			if event.Type != watch.Modified && event.Type != watch.Added {
				continue
			}
			updated, ok := event.Object.(*v1alpha1.AgentRequest)
			if !ok || updated.Name != requestID {
				continue
			}
			if result := s.aipApprovalResult(updated); result != nil {
				if wErr := mcp.WriteJSONRPCResponse(w, req.ID, *result); wErr != nil {
					log.Printf("WriteJSONRPCResponse failed: %v", wErr)
				}
				return
			}
		}
	}
}

// aipApprovalResult returns the MCP ToolsCallResult when the AgentRequest has
// reached a terminal phase, nil when still in-progress.
func (s *Server) aipApprovalResult(ar *v1alpha1.AgentRequest) *mcp.ToolsCallResult {
	switch ar.Status.Phase {
	case v1alpha1.PhaseApproved:
		var jwtToken string
		if s.jwtManager != nil {
			tok, _, err := s.jwtManager.MintToken(
				ar.Spec.AgentIdentity,
				ar.Spec.Action,
				ar.Spec.Target.URI,
				ar.Name,
			)
			if err != nil {
				log.Printf("failed to mint JWT for approved request %s: %v", ar.Name, err)
			} else {
				jwtToken = tok
			}
		}
		approvedBy := ""
		if ar.Spec.HumanApproval != nil {
			approvedBy = ar.Spec.HumanApproval.ApprovedBy
		}
		payload := map[string]string{
			"status":     "approved",
			"jwt":        jwtToken,
			"approvedBy": approvedBy,
			"requestId":  ar.Name,
			"message":    "Re-call the tool with _aip_authorization set to the jwt value.",
		}
		return &mcp.ToolsCallResult{Content: []json.RawMessage{aipTextContent(payload)}}

	case v1alpha1.PhaseDenied:
		reason := "denied by policy"
		if ar.Status.Denial != nil {
			reason = ar.Status.Denial.Message
		}
		payload := map[string]string{
			"status":    "denied",
			"reason":    reason,
			"requestId": ar.Name,
		}
		return &mcp.ToolsCallResult{Content: []json.RawMessage{aipTextContent(payload)}}

	case v1alpha1.PhaseExpired:
		payload := map[string]string{
			"status":    "expired",
			"requestId": ar.Name,
		}
		return &mcp.ToolsCallResult{Content: []json.RawMessage{aipTextContent(payload)}}

	case v1alpha1.PhaseCompleted:
		// Observer-level request: graded by a human reviewer, no execution occurred.
		// Verdict is always set before the controller transitions to Completed from AwaitingVerdict.
		if ar.Status.Verdict != "" {
			payload := map[string]any{
				"status":    "graded",
				"verdict":   ar.Status.Verdict,
				"requestId": ar.Name,
				"message": "Your diagnostic was reviewed. As an Observer, no action was executed. " +
					"Accuracy will be updated on your trust profile.",
			}
			if ar.Status.VerdictReasonCode != "" {
				payload["reasonCode"] = ar.Status.VerdictReasonCode
			}
			if ar.Status.VerdictNote != "" {
				payload["note"] = ar.Status.VerdictNote
			}
			return &mcp.ToolsCallResult{Content: []json.RawMessage{aipTextContent(payload)}}
		}

	case v1alpha1.PhaseFailed:
		payload := map[string]string{
			"status":    "failed",
			"requestId": ar.Name,
			"message":   "The request was abandoned or failed before a decision could be made.",
		}
		return &mcp.ToolsCallResult{Content: []json.RawMessage{aipTextContent(payload)}}
	}
	return nil
}

// enforceRegistrationPolicy resolves the effective agent identity and enforces
// registration policy. Returns (resolvedAgentID, unregistered, error).
func (s *Server) enforceRegistrationPolicy(ctx context.Context, agentID string) (string, bool, error) {
	if s.regCache == nil {
		return agentID, false, nil
	}
	sub := callerSubFromCtx(ctx)
	reg := s.regCache.getForSubject(agentID, sub)
	if reg == nil {
		if agentID != "" && s.regCache.exists(agentID) {
			return "", false, fmt.Errorf("%w: OIDC subject not in allowedSubjects", ErrIdentityMismatch)
		}
		switch s.unregisteredAgentPolicy {
		case policyStrict:
			return "", true, ErrAgentNotRegistered
		case policyWarn:
			log.Printf("Unregistered agent in MCP write tool path, policy=warn, agentIdentity=%q", agentID)
		}
		return agentID, true, nil
	}
	// getForSubject already verified identity (sub ∈ AllowedSubjects or sub == agentIdentity).
	// Guard against an empty sub when auth is required as a final safety net.
	if s.authRequired && sub == "" {
		return "", false, fmt.Errorf("%w: caller subject required when auth is enabled", ErrIdentityMismatch)
	}
	return reg.Spec.AgentIdentity, false, nil
}

// submitAgentRequestForTool creates an AgentRequest for a write tool call that
// lacks an AIP JWT, triggering governance evaluation by the controller.
func (s *Server) submitAgentRequestForTool(
	ctx context.Context,
	agentID, prefixedToolName, targetURI, reason string,
	toolArgs map[string]any,
) (*v1alpha1.AgentRequest, error) {
	if s.client == nil {
		return nil, fmt.Errorf("kubernetes client not configured")
	}

	resolvedID, unregistered, err := s.enforceRegistrationPolicy(ctx, agentID)
	if err != nil {
		return nil, err
	}
	agentID = resolvedID

	if targetURI == "" {
		targetURI = buildTargetURI(toolArgs)
	}
	if reason == "" {
		reason = "Agent tool call: " + prefixedToolName
	}

	ar := &v1alpha1.AgentRequest{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: defaultNamespace,
			Labels: map[string]string{
				"aip.io/agentIdentity": sanitizeLabelValue(agentID),
				"aip.io/profileName":   v1alpha1.ProfileNameForAgent(agentID),
			},
		},
		Spec: v1alpha1.AgentRequestSpec{
			AgentIdentity: agentID,
			Action:        prefixedToolName,
			Target:        v1alpha1.Target{URI: targetURI},
			Reason:        reason,
		},
	}

	if unregistered && s.unregisteredAgentPolicy == policyWarn {
		if ar.Annotations == nil {
			ar.Annotations = make(map[string]string)
		}
		ar.Annotations["governance.aip.io/unregistered"] = annotationValueTrue
	}

	// Serialize the tool args as AgentRequest parameters so SafetyPolicy CEL
	// expressions can access them (e.g. request.spec.parameters.replicas).
	if len(toolArgs) > 0 {
		paramsJSON, err := json.Marshal(toolArgs)
		if err != nil {
			log.Printf("submitAgentRequestForTool: failed to marshal tool args: %v", err)
		} else {
			ar.Spec.Parameters = &apiextensionsv1.JSON{Raw: paramsJSON}
		}
	}

	// Look up matching GovernedResource so the controller can scope policy evaluation
	// and so we can stamp trust gate annotations (Observer → AwaitingVerdict routing).
	var govResources v1alpha1.GovernedResourceList
	if err := s.client.List(ctx, &govResources); err == nil && len(govResources.Items) > 0 {
		if gr := matchGovernedResource(govResources.Items, targetURI, prefixedToolName); gr != nil {
			ar.Spec.GovernedResourceRef = &v1alpha1.GovernedResourceRef{
				Name:       gr.Name,
				Generation: gr.Generation,
			}
			if gr.Spec.TrustRequirements != nil {
				trustResult, terr := s.evaluateTrustGate(ctx, defaultNamespace, agentID, "", gr)
				if terr != nil {
					log.Printf("submitAgentRequestForTool: trust gate error for %s: %v", agentID, terr)
				} else if trustResult.rejected {
					// Insufficient trust for execution — stamp canExecute=false so
					// the controller routes this to AwaitingVerdict for diagnostic
					// grading rather than the normal approval flow.
					log.Printf("submitAgentRequestForTool: trust gate rejected %s (%s), routing to AwaitingVerdict",
						agentID, trustResult.message)
					if ar.Annotations == nil {
						ar.Annotations = make(map[string]string)
					}
					ar.Annotations[v1alpha1.AnnotationCanExecute] = "false"
				} else if len(trustResult.annotations) > 0 {
					log.Printf("submitAgentRequestForTool: trust gate passed for %s, annotations=%v", agentID, trustResult.annotations)
					if ar.Annotations == nil {
						ar.Annotations = make(map[string]string)
					}
					maps.Copy(ar.Annotations, trustResult.annotations)
				}
			} else {
				log.Printf("submitAgentRequestForTool: GovernedResource %s has no trustRequirements, skipping trust gate", gr.Name)
			}
		}
	}

	// Retry up to 3 times on name collision (4-byte random name: collision is
	// extremely unlikely but AlreadyExists must be handled separately from Conflict).
	for range 3 {
		ar.Name = mcpRequestName()
		if err := s.client.Create(ctx, ar); err != nil {
			if apierrors.IsAlreadyExists(err) {
				log.Printf("submitAgentRequestForTool: name collision %s, retrying", ar.Name)
				continue
			}
			return nil, fmt.Errorf("creating AgentRequest for tool call: %w", err)
		}
		return ar, nil
	}
	return nil, fmt.Errorf("creating AgentRequest: name collision after 3 attempts")
}

// pendingApprovalContent returns a text content block indicating the tool call
// is awaiting governance approval.
func pendingApprovalContent(requestID string) json.RawMessage {
	payload := map[string]string{
		"status":    "pending_approval",
		"requestId": requestID,
		"message":   "This action requires approval. Call aip/await_approval with the requestId to wait for the decision.",
	}
	return aipTextContent(payload)
}

func aipTextContent(v any) json.RawMessage {
	textJSON, _ := json.Marshal(v)
	encoded, _ := json.Marshal(string(textJSON))
	return json.RawMessage(`{"type":"text","text":` + string(encoded) + `}`)
}

func mcpRequestName() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		log.Printf("mcpRequestName: rand.Read failed: %v", err)
		return fmt.Sprintf("mcp-%08x", uint32(time.Now().UnixNano()))
	}
	return fmt.Sprintf("mcp-%x", b)
}

// completeAgentRequest advances a JWT-authorized AgentRequest through
// Executing → Completed so the controller releases the OpsLock.
// Called after a successful MCP tool execution.
func (s *Server) completeAgentRequest(ctx context.Context, name, outcome string) {
	if s.client == nil || name == "" {
		return
	}
	err := s.setARCondition(ctx, name, v1alpha1.ConditionExecuting, "AgentStarted", "Agent executing via MCP")
	if err != nil {
		log.Printf("completeAgentRequest: set Executing on %s: %v", name, err)
		return
	}
	msg := "Agent successfully completed the action"
	if outcome != "" {
		msg = outcome
	}
	if err := s.setARCondition(ctx, name, v1alpha1.ConditionCompleted, "ActionSuccess", msg); err != nil {
		log.Printf("completeAgentRequest: set Completed on %s: %v", name, err)
	}
}

// failAgentRequest moves a Pending or Approved AgentRequest to Failed,
// releasing any held OpsLock. Safe to call when the request is already terminal.
func (s *Server) failAgentRequest(ctx context.Context, name, reason string) {
	if s.client == nil || name == "" {
		return
	}
	// If still Pending, fail it directly.
	// If Approved, we must go through Executing first so the controller
	// transitions correctly and releases the lock.
	var ar v1alpha1.AgentRequest
	if err := s.client.Get(ctx, types.NamespacedName{Name: name, Namespace: defaultNamespace}, &ar); err != nil {
		return
	}
	switch ar.Status.Phase {
	case v1alpha1.PhaseApproved:
		err := s.setARCondition(ctx, name, v1alpha1.ConditionExecuting, "AgentStarted", "Agent executing via MCP")
		if err != nil {
			log.Printf("failAgentRequest: set Executing on %s: %v", name, err)
			return
		}
		fallthrough
	case v1alpha1.PhaseExecuting:
		if err := s.setARCondition(ctx, name, v1alpha1.ConditionFailed, "ExecutionFailed", reason); err != nil {
			log.Printf("failAgentRequest: set Failed on %s: %v", name, err)
		}
	case v1alpha1.PhasePending, v1alpha1.PhaseAwaitingVerdict:
		if err := s.setARCondition(ctx, name, v1alpha1.ConditionFailed, "Abandoned", reason); err != nil {
			log.Printf("failAgentRequest: set Failed on %s: %v", name, err)
		}
	}
}

// setARCondition patches a single status condition on an AgentRequest,
// retrying on conflict. Skips silently if the client is nil.
func (s *Server) setARCondition(ctx context.Context, name, condType, reason, msg string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ar v1alpha1.AgentRequest
		if err := s.client.Get(ctx, types.NamespacedName{Name: name, Namespace: defaultNamespace}, &ar); err != nil {
			return err
		}
		base := ar.DeepCopy()
		meta.SetStatusCondition(&ar.Status.Conditions, metav1.Condition{
			Type:    condType,
			Status:  metav1.ConditionTrue,
			Reason:  reason,
			Message: msg,
		})
		return s.client.Status().Patch(ctx, &ar, client.MergeFrom(base))
	})
}

// buildTargetURI constructs a k8s:// URI from well-known tool arguments.
func buildTargetURI(args map[string]any) string {
	ns, _ := args["namespace"].(string)
	if ns == "" {
		ns = "default"
	}
	// Prefer explicit name+kind (used by resources_scale, resources_get, etc.)
	name, _ := args["name"].(string)
	kind, _ := args["kind"].(string)
	if name != "" && kind != "" {
		return fmt.Sprintf("k8s://%s/%s/%s", ns, strings.ToLower(kind), name)
	}
	// Legacy: some tools pass a top-level deployment key.
	dep, _ := args["deployment"].(string)
	if dep != "" {
		return fmt.Sprintf("k8s://%s/deployment/%s", ns, dep)
	}
	return fmt.Sprintf("k8s://%s", ns)
}
