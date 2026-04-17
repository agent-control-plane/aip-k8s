package evaluation

import (
	"context"
	"testing"

	aipv1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ptr[T any](v T) *T { return &v }

func TestEvaluator(t *testing.T) {
	eval, err := NewEvaluator()
	if err != nil {
		t.Fatalf("Failed to create evaluator: %v", err)
	}

	tests := []struct {
		name         string
		req          *aipv1alpha1.AgentRequest
		policies     []aipv1alpha1.SafetyPolicy
		expectedAct  string
		expectedCode string
	}{
		{
			name: "Allow by default when no policies match",
			req: &aipv1alpha1.AgentRequest{
				Spec: aipv1alpha1.AgentRequestSpec{
					Target: aipv1alpha1.Target{URI: "k8s://prod/default/deployment/app"},
				},
			},
			policies:    []aipv1alpha1.SafetyPolicy{},
			expectedAct: "Allow",
		},
		{
			name: "Deny policy drops request",
			req: &aipv1alpha1.AgentRequest{
				Spec: aipv1alpha1.AgentRequestSpec{
					Target: aipv1alpha1.Target{URI: "k8s://prod/default/deployment/app"},
				},
			},
			policies: []aipv1alpha1.SafetyPolicy{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "block-prod"},
					Spec: aipv1alpha1.SafetyPolicySpec{
						Rules: []aipv1alpha1.Rule{
							{
								Name:       "deny-prod",
								Action:     "Deny",
								Expression: `request.spec.target.uri.startsWith("k8s://prod")`,
							},
						},
					},
				},
			},
			expectedAct:  "Deny",
			expectedCode: aipv1alpha1.DenialCodePolicyViolation,
		},
		{
			name: "Precedence: Deny overrides Allow",
			req: &aipv1alpha1.AgentRequest{
				Spec: aipv1alpha1.AgentRequestSpec{
					Target: aipv1alpha1.Target{URI: "k8s://prod/default/deployment/app"},
				},
			},
			policies: []aipv1alpha1.SafetyPolicy{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "allow-all"},
					Spec: aipv1alpha1.SafetyPolicySpec{
						Rules: []aipv1alpha1.Rule{
							{
								Name:       "allow-all",
								Action:     "Allow",
								Expression: `true`,
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "block-prod"},
					Spec: aipv1alpha1.SafetyPolicySpec{
						Rules: []aipv1alpha1.Rule{
							{
								Name:       "deny-prod",
								Action:     "Deny",
								Expression: `request.spec.target.uri.startsWith("k8s://prod")`,
							},
						},
					},
				},
			},
			expectedAct:  "Deny",
			expectedCode: aipv1alpha1.DenialCodePolicyViolation,
		},
		{
			name: "Precedence: RequireApproval overrides Allow",
			req: &aipv1alpha1.AgentRequest{
				Spec: aipv1alpha1.AgentRequestSpec{
					Target: aipv1alpha1.Target{URI: "k8s://staging/default/deployment/app"},
				},
			},
			policies: []aipv1alpha1.SafetyPolicy{
				{
					Spec: aipv1alpha1.SafetyPolicySpec{
						Rules: []aipv1alpha1.Rule{
							{
								Name:       "allow-all",
								Action:     "Allow",
								Expression: `true`,
							},
							{
								Name:       "require-approval-staging",
								Action:     "RequireApproval",
								Expression: `request.spec.target.uri.startsWith("k8s://staging")`,
							},
						},
					},
				},
			},
			expectedAct: "RequireApproval",
		},
		{
			name: "FailClosed semantics on bad CEL expression",
			req: &aipv1alpha1.AgentRequest{
				Spec: aipv1alpha1.AgentRequestSpec{
					Target: aipv1alpha1.Target{URI: "k8s://staging/default"},
				},
			},
			policies: []aipv1alpha1.SafetyPolicy{
				{
					Spec: aipv1alpha1.SafetyPolicySpec{
						FailureMode: ptr("FailClosed"),
						Rules: []aipv1alpha1.Rule{
							{
								Name:       "bad-expression",
								Action:     "Allow",
								Expression: `request.spec.target.does_not_exist == true`,
							},
						},
					},
				},
			},
			expectedAct:  "Deny",
			expectedCode: aipv1alpha1.DenialCodeEvaluationFailure,
		},
		{
			name: "FailOpen semantics on bad CEL expression",
			req: &aipv1alpha1.AgentRequest{
				Spec: aipv1alpha1.AgentRequestSpec{
					Target: aipv1alpha1.Target{URI: "k8s://staging/default"},
				},
			},
			policies: []aipv1alpha1.SafetyPolicy{
				{
					Spec: aipv1alpha1.SafetyPolicySpec{
						FailureMode: ptr("FailOpen"),
						Rules: []aipv1alpha1.Rule{
							{
								Name:       "bad-expression",
								Action:     "Deny",
								Expression: `request.spec.target.does_not_exist == true`,
							},
						},
					},
				},
			},
			expectedAct: "Allow",
		},
		{
			name: "Cascade Model checking (deny)",
			req: &aipv1alpha1.AgentRequest{
				Spec: aipv1alpha1.AgentRequestSpec{
					Target: aipv1alpha1.Target{URI: "k8s://staging/default/payment-api"},
					CascadeModel: &aipv1alpha1.CascadeModel{
						AffectedTargets: []aipv1alpha1.AffectedTarget{
							{URI: "k8s://prod/default/payment-db", EffectType: "disrupted"},
						},
					},
				},
			},
			policies: []aipv1alpha1.SafetyPolicy{
				{
					Spec: aipv1alpha1.SafetyPolicySpec{
						Rules: []aipv1alpha1.Rule{
							{
								Name:       "protect-prod-db",
								Action:     "Deny",
								Expression: `request.spec.target.uri == "k8s://prod/default/payment-db" && request.spec.action == "disrupted"`,
							},
						},
					},
				},
			},
			expectedAct:  "Deny",
			expectedCode: aipv1alpha1.DenialCodeCascadeDenied,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := eval.Evaluate(context.Background(), tc.req, tc.policies, nil, nil)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if res.Action != tc.expectedAct {
				t.Errorf("Expected Action %s, got %s", tc.expectedAct, res.Action)
			}
			if tc.expectedCode != "" && res.Code != tc.expectedCode {
				t.Errorf("Expected Code %s, got %s", tc.expectedCode, res.Code)
			}
		})
	}
}
