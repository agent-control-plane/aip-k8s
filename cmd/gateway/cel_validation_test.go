package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

func TestCEL_ValidExpressionAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	schemaRaw := []byte(`{"type":"object","properties":{"cpuCount":{"type":"integer"}}}`)
	gr := &v1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: "gr-1"},
		Spec: v1alpha1.GovernedResourceSpec{
			ContextFetcher: "karpenter",
			ContextSchema:  &apiextensionsv1.JSON{Raw: schemaRaw},
		},
	}
	s := newAdminTestServer(gr)

	sp := &v1alpha1.SafetyPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "sp-1"},
		Spec: v1alpha1.SafetyPolicySpec{
			ContextType: "karpenter",
			Rules: []v1alpha1.Rule{{
				Name:       "rule1",
				Type:       "StateEvaluation",
				Action:     "Allow",
				Expression: "ctxData.cpuCount < 10",
			}},
		},
	}
	body, _ := json.Marshal(sp)
	req := httptest.NewRequest(http.MethodPost, "/safety-policies", bytes.NewReader(body))
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleCreateSafetyPolicy(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusCreated))
}

func TestCEL_InvalidFieldRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	schemaRaw := []byte(`{"type":"object","properties":{"cpuCount":{"type":"integer"}}}`)
	gr := &v1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: "gr-1"},
		Spec: v1alpha1.GovernedResourceSpec{
			ContextFetcher: "karpenter",
			ContextSchema:  &apiextensionsv1.JSON{Raw: schemaRaw},
		},
	}
	s := newAdminTestServer(gr)

	sp := &v1alpha1.SafetyPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "sp-1"},
		Spec: v1alpha1.SafetyPolicySpec{
			ContextType: "karpenter",
			Rules: []v1alpha1.Rule{{
				Name:       "rule1",
				Type:       "StateEvaluation",
				Action:     "Allow",
				Expression: "ctxData.nonExistentField == true",
			}},
		},
	}
	body, _ := json.Marshal(sp)
	req := httptest.NewRequest(http.MethodPost, "/safety-policies", bytes.NewReader(body))
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleCreateSafetyPolicy(w, req)

	g.Expect(w.Code).To(gomega.Equal(422))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring("undefined field 'nonExistentField'"))
}

func TestCEL_EmptyContextType_SkipsCheck(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newAdminTestServer()

	// With no ContextType, CEL is still validated against the base env (request + target).
	// Expressions that only reference request/target should be accepted.
	sp := &v1alpha1.SafetyPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "sp-1"},
		Spec: v1alpha1.SafetyPolicySpec{
			ContextType: "",
			Rules: []v1alpha1.Rule{{
				Name:       "rule1",
				Type:       "StateEvaluation",
				Action:     "Allow",
				Expression: `request.spec.target.uri.startsWith("k8s://")`,
			}},
		},
	}
	body, _ := json.Marshal(sp)
	req := httptest.NewRequest(http.MethodPost, "/safety-policies", bytes.NewReader(body))
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleCreateSafetyPolicy(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusCreated))
}

func TestCEL_NoMatchingGR_SkipsCheck(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newAdminTestServer()

	// When ContextType is set but no matching GR exists, CEL is still validated
	// against the base env (request + target). An expression that only uses
	// request/target must be accepted; ctxData would be unknown and rejected.
	sp := &v1alpha1.SafetyPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "sp-1"},
		Spec: v1alpha1.SafetyPolicySpec{
			ContextType: "karpenter",
			Rules: []v1alpha1.Rule{{
				Name:       "rule1",
				Type:       "StateEvaluation",
				Action:     "Allow",
				Expression: `request.spec.target.uri.startsWith("k8s://")`,
			}},
		},
	}
	body, _ := json.Marshal(sp)
	req := httptest.NewRequest(http.MethodPost, "/safety-policies", bytes.NewReader(body))
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleCreateSafetyPolicy(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusCreated))
}
