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

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/evaluation"
)

func setupScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = governancev1alpha1.AddToScheme(s)
	return s
}

func TestGovernedResourceRef_DeletedGR_DeniesRequest(t *testing.T) {
	g := gomega.NewWithT(t)
	scheme := setupScheme()

	// AgentRequest referencing a missing GovernedResource
	ar := &governancev1alpha1.AgentRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "req1", Namespace: "default"},
		Spec: governancev1alpha1.AgentRequestSpec{
			GovernedResourceRef: &governancev1alpha1.GovernedResourceRef{Name: "missing"},
		},
	}
	ar.Status.Phase = governancev1alpha1.PhasePending

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ar).
		WithStatusSubresource(&governancev1alpha1.AgentRequest{}).
		Build()

	eval, _ := evaluation.NewEvaluator()
	reconciler := &AgentRequestReconciler{
		Client:               fc,
		Scheme:               scheme,
		Evaluator:            eval,
		TargetContextFetcher: &evaluation.KubernetesTargetContextFetcher{Client: fc},
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "req1", Namespace: "default"},
	})
	g.Expect(err).To(gomega.Succeed())

	var updated governancev1alpha1.AgentRequest
	g.Expect(fc.Get(context.Background(), types.NamespacedName{Name: "req1", Namespace: "default"}, &updated)).To(gomega.Succeed())
	g.Expect(updated.Status.Phase).To(gomega.Equal(governancev1alpha1.PhaseDenied))
	g.Expect(updated.Status.Denial).NotTo(gomega.BeNil())
	g.Expect(updated.Status.Denial.Code).To(gomega.Equal(governancev1alpha1.DenialCodeGovernedResourceDeleted))
}

func TestGovernedResourceRef_ExistingGR_NoEffect(t *testing.T) {
	g := gomega.NewWithT(t)
	scheme := setupScheme()

	gr := &governancev1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: "gr1"},
		Spec:       governancev1alpha1.GovernedResourceSpec{URIPattern: "k8s://*"},
	}
	ar := &governancev1alpha1.AgentRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "req1", Namespace: "default"},
		Spec: governancev1alpha1.AgentRequestSpec{
			GovernedResourceRef: &governancev1alpha1.GovernedResourceRef{Name: "gr1"},
		},
	}
	ar.Status.Phase = governancev1alpha1.PhasePending

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gr, ar).
		WithStatusSubresource(&governancev1alpha1.AgentRequest{}).
		Build()

	eval, _ := evaluation.NewEvaluator()
	reconciler := &AgentRequestReconciler{
		Client:               fc,
		Scheme:               scheme,
		Evaluator:            eval,
		TargetContextFetcher: &evaluation.KubernetesTargetContextFetcher{Client: fc},
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "req1", Namespace: "default"},
	})
	g.Expect(err).To(gomega.Succeed())

	var updated governancev1alpha1.AgentRequest
	g.Expect(fc.Get(context.Background(), types.NamespacedName{Name: "req1", Namespace: "default"}, &updated)).To(gomega.Succeed())
	// Should NOT be denied; should proceed to Approved if no policies block it
	g.Expect(updated.Status.Phase).To(gomega.Equal(governancev1alpha1.PhaseApproved))
}

func TestFinalizer_AddedWhenActiveRequest(t *testing.T) {
	g := gomega.NewWithT(t)
	scheme := setupScheme()

	gr := &governancev1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: "gr1"},
	}
	ar := &governancev1alpha1.AgentRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "req1", Namespace: "default"},
		Spec: governancev1alpha1.AgentRequestSpec{
			GovernedResourceRef: &governancev1alpha1.GovernedResourceRef{Name: "gr1"},
		},
	}
	ar.Status.Phase = governancev1alpha1.PhasePending

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gr, ar).
		Build()

	reconciler := &GovernedResourceReconciler{
		Client: fc,
		Scheme: scheme,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gr1"},
	})
	g.Expect(err).To(gomega.Succeed())

	var updated governancev1alpha1.GovernedResource
	g.Expect(fc.Get(context.Background(), types.NamespacedName{Name: "gr1"}, &updated)).To(gomega.Succeed())
	g.Expect(updated.Finalizers).To(gomega.ContainElement(governancev1alpha1.GovernedResourceFinalizer))
}

func TestFinalizer_DeletionBlockedByActiveRequest(t *testing.T) {
	g := gomega.NewWithT(t)
	scheme := setupScheme()

	now := metav1.NewTime(time.Now())
	gr := &governancev1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "gr1",
			DeletionTimestamp: &now,
			Finalizers:        []string{governancev1alpha1.GovernedResourceFinalizer},
		},
	}
	ar := &governancev1alpha1.AgentRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "req1", Namespace: "default"},
		Spec: governancev1alpha1.AgentRequestSpec{
			GovernedResourceRef: &governancev1alpha1.GovernedResourceRef{Name: "gr1"},
		},
	}
	ar.Status.Phase = governancev1alpha1.PhasePending

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gr, ar).
		Build()

	reconciler := &GovernedResourceReconciler{
		Client: fc,
		Scheme: scheme,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gr1"},
	})
	g.Expect(err).To(gomega.Succeed())
	// Deletion is blocked — rely on watch events from AgentRequest changes,
	// not an immediate requeue. Result should be zero-value (no requeue).
	g.Expect(result).To(gomega.Equal(ctrl.Result{}))

	var updated governancev1alpha1.GovernedResource
	g.Expect(fc.Get(context.Background(), types.NamespacedName{Name: "gr1"}, &updated)).To(gomega.Succeed())
	g.Expect(updated.Finalizers).To(gomega.ContainElement(governancev1alpha1.GovernedResourceFinalizer))
}

func TestFinalizer_RemovedWhenNoActiveRequests(t *testing.T) {
	g := gomega.NewWithT(t)
	scheme := setupScheme()

	now := metav1.NewTime(time.Now())
	gr := &governancev1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "gr1",
			DeletionTimestamp: &now,
			Finalizers:        []string{governancev1alpha1.GovernedResourceFinalizer},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gr).
		Build()

	reconciler := &GovernedResourceReconciler{
		Client: fc,
		Scheme: scheme,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gr1"},
	})
	g.Expect(err).To(gomega.Succeed())

	var updated governancev1alpha1.GovernedResource
	err = fc.Get(context.Background(), types.NamespacedName{Name: "gr1"}, &updated)
	if err == nil {
		g.Expect(updated.Finalizers).NotTo(gomega.ContainElement(governancev1alpha1.GovernedResourceFinalizer))
	} else {
		g.Expect(errors.IsNotFound(err)).To(gomega.BeTrue())
	}
}
