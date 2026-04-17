package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

func newAdminTestServer(objs ...client.Object) *Server {
	s := newTestServer(objs...)
	s.roles = newRoleConfig("agent-sub", "reviewer-sub", "admin-sub", "", "", "")
	return s
}

func TestAdmin_CreateGovernedResource_OK(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newAdminTestServer()

	gr := &v1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: "new-gr"},
		Spec: v1alpha1.GovernedResourceSpec{
			URIPattern: "k8s://prod/*",
		},
	}
	body, _ := json.Marshal(gr)
	req := httptest.NewRequest(http.MethodPost, "/governed-resources", bytes.NewReader(body))
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleCreateGovernedResource(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusCreated))

	var fetched v1alpha1.GovernedResource
	err := s.client.Get(context.Background(), types.NamespacedName{Name: "new-gr"}, &fetched)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestAdmin_CreateGovernedResource_NonAdminRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newAdminTestServer()

	req := httptest.NewRequest(http.MethodPost, "/governed-resources", bytes.NewReader([]byte("{}")))
	req = req.WithContext(withCallerSub(req.Context(), "agent-sub"))
	w := httptest.NewRecorder()
	s.handleCreateGovernedResource(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusForbidden))
}

func TestAdmin_ListGovernedResources_OK(t *testing.T) {
	g := gomega.NewWithT(t)
	gr1 := &v1alpha1.GovernedResource{ObjectMeta: metav1.ObjectMeta{Name: "gr-1"}}
	gr2 := &v1alpha1.GovernedResource{ObjectMeta: metav1.ObjectMeta{Name: "gr-2"}}
	s := newAdminTestServer(gr1, gr2)

	req := httptest.NewRequest(http.MethodGet, "/governed-resources", nil)
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleListGovernedResources(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
	var list v1alpha1.GovernedResourceList
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	g.Expect(list.Items).To(gomega.HaveLen(2))
}

func TestAdmin_GetGovernedResource_NotFound(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newAdminTestServer()

	req := httptest.NewRequest(http.MethodGet, "/governed-resources/missing", nil)
	req.SetPathValue("name", "missing")
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleGetGovernedResource(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusNotFound))
}

func TestAdmin_ReplaceGovernedResource_OK(t *testing.T) {
	g := gomega.NewWithT(t)
	gr := &v1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: "gr-1"},
		Spec:       v1alpha1.GovernedResourceSpec{URIPattern: "old"},
	}
	s := newAdminTestServer(gr)

	updatedGR := &v1alpha1.GovernedResource{
		Spec: v1alpha1.GovernedResourceSpec{URIPattern: "new"},
	}
	body, _ := json.Marshal(updatedGR)
	req := httptest.NewRequest(http.MethodPut, "/governed-resources/gr-1", bytes.NewReader(body))
	req.SetPathValue("name", "gr-1")
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleReplaceGovernedResource(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
	var fetched v1alpha1.GovernedResource
	_ = s.client.Get(context.Background(), types.NamespacedName{Name: "gr-1"}, &fetched)
	g.Expect(fetched.Spec.URIPattern).To(gomega.Equal("new"))
}

func TestAdmin_ReplaceGovernedResource_NotFound(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newAdminTestServer()

	req := httptest.NewRequest(http.MethodPut, "/governed-resources/missing", bytes.NewReader([]byte("{}")))
	req.SetPathValue("name", "missing")
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleReplaceGovernedResource(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusNotFound))
}

func TestAdmin_DeleteGovernedResource_OK(t *testing.T) {
	g := gomega.NewWithT(t)
	gr := &v1alpha1.GovernedResource{ObjectMeta: metav1.ObjectMeta{Name: "gr-1"}}
	s := newAdminTestServer(gr)

	req := httptest.NewRequest(http.MethodDelete, "/governed-resources/gr-1", nil)
	req.SetPathValue("name", "gr-1")
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleDeleteGovernedResource(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusNoContent))
	var fetched v1alpha1.GovernedResource
	err := s.client.Get(context.Background(), types.NamespacedName{Name: "gr-1"}, &fetched)
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
}

func TestAdmin_CreateSafetyPolicy_OK(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newAdminTestServer()

	sp := &v1alpha1.SafetyPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "new-sp"},
		Spec: v1alpha1.SafetyPolicySpec{
			GovernedResourceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"foo": "bar"}},
			Rules: []v1alpha1.Rule{{
				Name:       "rule1",
				Type:       "StateEvaluation",
				Action:     "Allow",
				Expression: "true",
			}},
		},
	}
	body, _ := json.Marshal(sp)
	req := httptest.NewRequest(http.MethodPost, "/safety-policies?namespace=custom", bytes.NewReader(body))
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleCreateSafetyPolicy(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusCreated))
	var fetched v1alpha1.SafetyPolicy
	err := s.client.Get(context.Background(), types.NamespacedName{Name: "new-sp", Namespace: "custom"}, &fetched)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestAdmin_ReplaceSafetyPolicy_OK(t *testing.T) {
	g := gomega.NewWithT(t)
	sp := &v1alpha1.SafetyPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "sp-1", Namespace: "default"},
		Spec:       v1alpha1.SafetyPolicySpec{ContextType: "old"},
	}
	s := newAdminTestServer(sp)

	updatedSP := &v1alpha1.SafetyPolicy{
		Spec: v1alpha1.SafetyPolicySpec{ContextType: "new"},
	}
	body, _ := json.Marshal(updatedSP)
	req := httptest.NewRequest(http.MethodPut, "/safety-policies/sp-1", bytes.NewReader(body))
	req.SetPathValue("name", "sp-1")
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleReplaceSafetyPolicy(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
	var fetched v1alpha1.SafetyPolicy
	_ = s.client.Get(context.Background(), types.NamespacedName{Name: "sp-1", Namespace: "default"}, &fetched)
	g.Expect(fetched.Spec.ContextType).To(gomega.Equal("new"))
}

func TestAdmin_DeleteSafetyPolicy_OK(t *testing.T) {
	g := gomega.NewWithT(t)
	sp := &v1alpha1.SafetyPolicy{ObjectMeta: metav1.ObjectMeta{Name: "sp-1", Namespace: "default"}}
	s := newAdminTestServer(sp)

	req := httptest.NewRequest(http.MethodDelete, "/safety-policies/sp-1", nil)
	req.SetPathValue("name", "sp-1")
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleDeleteSafetyPolicy(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusNoContent))
}

func TestAdmin_NonAdminCannotCreateSafetyPolicy(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newAdminTestServer()

	req := httptest.NewRequest(http.MethodPost, "/safety-policies", bytes.NewReader([]byte("{}")))
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w := httptest.NewRecorder()
	s.handleCreateSafetyPolicy(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusForbidden))
}

func TestAdmin_SchemaConsistency_FirstGRAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newAdminTestServer()

	gr := &v1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: "gr-1"},
		Spec: v1alpha1.GovernedResourceSpec{
			ContextFetcher: "karpenter",
			ContextSchema:  &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"cpuCount":{"type":"integer"}}}`)},
		},
	}
	body, _ := json.Marshal(gr)
	req := httptest.NewRequest(http.MethodPost, "/governed-resources", bytes.NewReader(body))
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleCreateGovernedResource(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusCreated))
}

func TestAdmin_SchemaConsistency_IdenticalSchemaAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	gr1 := &v1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: "gr-1"},
		Spec: v1alpha1.GovernedResourceSpec{
			ContextFetcher: "karpenter",
			ContextSchema:  &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"cpuCount":{"type":"integer"}}}`)},
		},
	}
	s := newAdminTestServer(gr1)

	gr2 := &v1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: "gr-2"},
		Spec: v1alpha1.GovernedResourceSpec{
			ContextFetcher: "karpenter",
			ContextSchema:  &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"cpuCount":{"type":"integer"}}}`)},
		},
	}
	body, _ := json.Marshal(gr2)
	req := httptest.NewRequest(http.MethodPost, "/governed-resources", bytes.NewReader(body))
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleCreateGovernedResource(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusCreated))
}

func TestAdmin_SchemaConsistency_DifferentSchemaRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	gr1 := &v1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: "gr-1"},
		Spec: v1alpha1.GovernedResourceSpec{
			ContextFetcher: "karpenter",
			ContextSchema:  &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"cpuCount":{"type":"integer"}}}`)},
		},
	}
	s := newAdminTestServer(gr1)

	gr2 := &v1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: "gr-2"},
		Spec: v1alpha1.GovernedResourceSpec{
			ContextFetcher: "karpenter",
			ContextSchema: &apiextensionsv1.JSON{Raw: []byte(
				`{"type":"object","properties":{"cpuCount":{"type":"string"}}}`,
			)}, // was integer
		},
	}
	body, _ := json.Marshal(gr2)
	req := httptest.NewRequest(http.MethodPost, "/governed-resources", bytes.NewReader(body))
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleCreateGovernedResource(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusConflict))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring("contextSchema evolution incompatible"))
}

func TestAdmin_AppendOnly_AddFieldAllowed(t *testing.T) {
	g := gomega.NewWithT(t)
	gr := &v1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: "gr-1"},
		Spec: v1alpha1.GovernedResourceSpec{
			ContextSchema: &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"f1":{"type":"integer"}}}`)},
		},
	}
	s := newAdminTestServer(gr)

	updatedGR := &v1alpha1.GovernedResource{
		Spec: v1alpha1.GovernedResourceSpec{
			ContextSchema: &apiextensionsv1.JSON{Raw: []byte(
				`{"type":"object","properties":{"f1":{"type":"integer"},"f2":{"type":"string"}}}`,
			)},
		},
	}
	body, _ := json.Marshal(updatedGR)
	req := httptest.NewRequest(http.MethodPut, "/governed-resources/gr-1", bytes.NewReader(body))
	req.SetPathValue("name", "gr-1")
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleReplaceGovernedResource(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
}

func TestAdmin_AppendOnly_RemoveFieldRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	gr := &v1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: "gr-1"},
		Spec: v1alpha1.GovernedResourceSpec{
			ContextSchema: &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"f1":{"type":"integer"}}}`)},
		},
	}
	s := newAdminTestServer(gr)

	updatedGR := &v1alpha1.GovernedResource{
		Spec: v1alpha1.GovernedResourceSpec{
			ContextSchema: &apiextensionsv1.JSON{Raw: []byte(
				`{"type":"object","properties":{"f2":{"type":"string"}}}`,
			)}, // f1 removed
		},
	}
	body, _ := json.Marshal(updatedGR)
	req := httptest.NewRequest(http.MethodPut, "/governed-resources/gr-1", bytes.NewReader(body))
	req.SetPathValue("name", "gr-1")
	req = req.WithContext(withCallerSub(req.Context(), "admin-sub"))
	w := httptest.NewRecorder()
	s.handleReplaceGovernedResource(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusConflict))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring("f1"))
}
