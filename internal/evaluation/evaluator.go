package evaluation

import (
	"context"

	aipv1alpha1 "github.com/ravisantoshgudimetla/aip-k8s/api/v1alpha1"
)

const failModeClosed = "FailClosed"

// Evaluator evaluates an AgentRequest against a list of SafetyPolicies.
// targetCtx holds live cluster state fetched by the control plane — it is
// available as the `target` variable in CEL expressions.
type Evaluator interface {
	Evaluate(ctx context.Context, req *aipv1alpha1.AgentRequest, policies []aipv1alpha1.SafetyPolicy, targetCtx *TargetContext) (*Result, error)
}

// Result is the deterministic outcome of evaluating policies
type Result struct {
	Action        string // "Allow", "Deny", "RequireApproval"
	Code          string // Only populated if Action == "Deny"
	Message       string
	PolicyResults []aipv1alpha1.PolicyResult
	// Optionally return an audit annotation map, etc.
}

// Action Precedence map to resolve conflicts. Higher number == higher priority.
var actionPriority = map[string]int{
	aipv1alpha1.ResultAllow:           1,
	"Log":                             2,
	aipv1alpha1.ResultRequireApproval: 3,
	aipv1alpha1.ResultDeny:            4,
}

type defaultEvaluator struct {
	env *CELEnvironment
}

// NewEvaluator creates a new SafetyPolicy evaluator
func NewEvaluator() (Evaluator, error) {
	env, err := NewCELEnvironment()
	if err != nil {
		return nil, err
	}
	return &defaultEvaluator{
		env: env,
	}, nil
}

// Evaluate applies strict conflict resolution precedence and FailureMode semantics.
// targetCtx is injected as the `target` CEL variable — live cluster state the
// control plane fetched independently of the agent.
func (e *defaultEvaluator) Evaluate(ctx context.Context, req *aipv1alpha1.AgentRequest, policies []aipv1alpha1.SafetyPolicy, targetCtx *TargetContext) (*Result, error) {
	result := &Result{
		Action:        aipv1alpha1.ResultAllow, // Default action if no rules apply
		PolicyResults: []aipv1alpha1.PolicyResult{},
	}

	highestPriority := actionPriority[aipv1alpha1.ResultAllow]
	denialCode := ""
	var finalMessage string

	// Create request+target variable map once for all rules
	reqVars, err := e.env.PrepareVariables(req, targetCtx)
	if err != nil {
		// If we can't even serialize the request, fail close.
		return &Result{
			Action:  aipv1alpha1.ResultDeny,
			Code:    aipv1alpha1.DenialCodeEvaluationFailure,
			Message: "Failed to parse AgentRequest variables for evaluation",
		}, nil
	}

	for _, policy := range policies {
		for _, rule := range policy.Spec.Rules {
			match, err := e.env.EvaluateExpression(rule.Expression, reqVars)

			pr := aipv1alpha1.PolicyResult{
				PolicyName: policy.Name,
				RuleName:   rule.Name,
				Result:     "Allow", // default for non-matching
			}

			if err != nil {
				// Evaluation Failure
				pr.Result = "EvaluationFailed"

				// Handle FailureMode
				failureMode := failModeClosed
				if policy.Spec.FailureMode != nil {
					failureMode = *policy.Spec.FailureMode
				}

				if failureMode == failModeClosed {
					if highestPriority < actionPriority[aipv1alpha1.ResultDeny] {
						highestPriority = actionPriority[aipv1alpha1.ResultDeny]
						result.Action = aipv1alpha1.ResultDeny
						denialCode = aipv1alpha1.DenialCodeEvaluationFailure
						finalMessage = "CEL evaluation failed (FailClosed): " + err.Error()
					}
					pr.Result = "EvaluationFailed (Deny)"
				} else {
					// FailOpen -> Log as EvaluationFailed but do not elevate priority
					if pr.Result != "EvaluationFailed" {
						pr.Result = "EvaluationFailed (Log)"
					}
				}
				result.PolicyResults = append(result.PolicyResults, pr)
				continue
			}

			if match {
				pr.Result = rule.Action

				// Update overall result if this rule has higher priority
				prio := actionPriority[rule.Action]
				if prio > highestPriority {
					highestPriority = prio
					result.Action = rule.Action

					switch rule.Action {
					case aipv1alpha1.ResultDeny:
						denialCode = aipv1alpha1.DenialCodePolicyViolation
						if rule.Message != nil {
							finalMessage = *rule.Message
						} else {
							finalMessage = "Denied by policy: " + policy.Name + " rule: " + rule.Name
						}
					case aipv1alpha1.ResultRequireApproval:
						if rule.Message != nil {
							finalMessage = *rule.Message
						} else {
							finalMessage = "Approval required by policy: " + policy.Name + " rule: " + rule.Name
						}
					}
				}
			}

			// Only append policies that matched or failed (we could append all, but that pollutes logs)
			if pr.Result != "Allow" && pr.Result != "" {
				result.PolicyResults = append(result.PolicyResults, pr)
			}
		}
	}

	_, denialCode, finalMessage = e.evaluateCascade(req, policies, result, highestPriority, denialCode, finalMessage)

	if result.Action == aipv1alpha1.ResultDeny {
		result.Code = denialCode
		result.Message = finalMessage
	} else if result.Action == "RequireApproval" {
		result.Message = finalMessage
	} else if result.Action == "Allow" && finalMessage == "" {
		result.Message = "All policies passed"
	}

	return result, nil
}

func (e *defaultEvaluator) evaluateCascade(req *aipv1alpha1.AgentRequest, policies []aipv1alpha1.SafetyPolicy, result *Result, highestPriority int, denialCode, finalMessage string) (int, string, string) {
	if req.Spec.CascadeModel == nil || len(req.Spec.CascadeModel.AffectedTargets) == 0 {
		return highestPriority, denialCode, finalMessage
	}
	for _, affectedTarget := range req.Spec.CascadeModel.AffectedTargets {
		// For cascade targets, we map them as temporary requests? Or we use a specific variable?
		// The simplest way is to evaluate the SafetyPolicies against a mock request that represents the affected target.
		// Let's create a mock variable map.
		cascadeVars := map[string]any{
			"request": map[string]any{
				"spec": map[string]any{
					"action": affectedTarget.EffectType,
					"target": map[string]any{
						"uri": affectedTarget.URI,
					},
				},
			},
		}

		for _, policy := range policies {
			for _, rule := range policy.Spec.Rules {
				match, err := e.env.EvaluateExpression(rule.Expression, cascadeVars)

				// Simple handle for FailClosed
				if err != nil {
					failureMode := failModeClosed
					if policy.Spec.FailureMode != nil {
						failureMode = *policy.Spec.FailureMode
					}
					if failureMode == failModeClosed {
						if highestPriority < actionPriority[aipv1alpha1.ResultDeny] {
							highestPriority = actionPriority[aipv1alpha1.ResultDeny]
							result.Action = aipv1alpha1.ResultDeny
							denialCode = aipv1alpha1.DenialCodeEvaluationFailure
							finalMessage = "Cascade CEL evaluation failed (FailClosed): " + err.Error()
						}
						result.PolicyResults = append(result.PolicyResults, aipv1alpha1.PolicyResult{
							PolicyName: policy.Name,
							RuleName:   rule.Name,
							Result:     "EvaluationFailed (Cascade Deny)",
						})
					}
					continue
				}

				if match && rule.Action == aipv1alpha1.ResultDeny {
					if highestPriority < actionPriority[aipv1alpha1.ResultDeny] {
						highestPriority = actionPriority[aipv1alpha1.ResultDeny]
						result.Action = aipv1alpha1.ResultDeny
						denialCode = aipv1alpha1.DenialCodeCascadeDenied
						finalMessage = "Cascade effect denied by policy: " + policy.Name + " rule: " + rule.Name
					}
					result.PolicyResults = append(result.PolicyResults, aipv1alpha1.PolicyResult{
						PolicyName: policy.Name,
						RuleName:   rule.Name,
						Result:     "CascadeDeny",
					})
				}
			}
		}
	}
	return highestPriority, denialCode, finalMessage
}
