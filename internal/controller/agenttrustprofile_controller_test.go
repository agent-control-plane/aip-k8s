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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

// fixedTestTime is used as the injected clock for all trust profile tests so that
// grace-period assertions are deterministic regardless of wall-clock time.
var fixedTestTime = time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

var _ = Describe("AgentTrustProfile Controller", Ordered, func() {
	// The "default" graduation policy is cluster-scoped and shared across all tests.
	// It is created once in BeforeAll and deleted in AfterAll.
	// GracePeriod is "1h" so that we can test both the in-grace and out-of-grace paths
	// purely by controlling profile.Status.LastPromotedAt relative to fixedTestTime.
	// newReconciler returns a fresh AgentTrustProfileReconciler backed by the
	// global k8sClient (direct, uncached). The injected clock always returns
	// fixedTestTime for deterministic grace-period assertions.
	newReconciler := func() *AgentTrustProfileReconciler {
		return &AgentTrustProfileReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Clock:     func() time.Time { return fixedTestTime },
		}
	}

	// createNamespace creates a uniquely named namespace and registers cleanup.
	createNamespace := func(prefix string) string {
		ns := prefix + fmt.Sprintf("-%d", time.Now().UnixNano())
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())

		// Create a test policy in the new namespace
		nsPolicy := &governancev1alpha1.AgentGraduationPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "default",
				Namespace: ns,
			},
			Spec: governancev1alpha1.AgentGraduationPolicySpec{
				EvaluationWindow: governancev1alpha1.EvaluationWindow{Count: 5},
				DemotionPolicy: governancev1alpha1.DemotionPolicy{
					GracePeriod: "1h",
					WindowSize:  10,
				},
				Levels: []governancev1alpha1.GraduationLevel{
					{
						Name:                  governancev1alpha1.TrustLevelObserver,
						CanExecute:            false,
						RequiresHumanApproval: true,
					},
					{
						Name:                  governancev1alpha1.TrustLevelAdvisor,
						CanExecute:            false,
						RequiresHumanApproval: true,
						Accuracy: &governancev1alpha1.AccuracyBand{
							Min:            ptr.To(0.7),
							DemotionBuffer: ptr.To(0.05),
						},
					},
					{
						Name:                  governancev1alpha1.TrustLevelSupervised,
						CanExecute:            true,
						RequiresHumanApproval: true,
						Accuracy: &governancev1alpha1.AccuracyBand{
							Min:            ptr.To(0.8),
							DemotionBuffer: ptr.To(0.05),
						},
					},
					{
						Name:                  governancev1alpha1.TrustLevelTrusted,
						CanExecute:            true,
						RequiresHumanApproval: false,
						Accuracy: &governancev1alpha1.AccuracyBand{
							Min:            ptr.To(0.9),
							DemotionBuffer: ptr.To(0.05),
						},
						Executions: &governancev1alpha1.ExecutionBand{
							Min: ptr.To(int64(5)),
						},
					},
					{
						Name:                  governancev1alpha1.TrustLevelAutonomous,
						CanExecute:            true,
						RequiresHumanApproval: false,
						Accuracy: &governancev1alpha1.AccuracyBand{
							Min:            ptr.To(0.95),
							DemotionBuffer: ptr.To(0.02),
						},
						Executions: &governancev1alpha1.ExecutionBand{
							Min: ptr.To(int64(10)),
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, nsPolicy)).To(Succeed())

		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, nsPolicy)
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		return ns
	}

	// createAuditRecord creates an AuditRecord with the verdict label required by
	// computeRollingAccuracy's label-selector query.
	createAuditRecord := func(ns, agentID, name, verdict string, ts time.Time) {
		ar := &governancev1alpha1.AuditRecord{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				Labels:    map[string]string{"aip.io/agentIdentity": agentID},
			},
			Spec: governancev1alpha1.AuditRecordSpec{
				Timestamp:       metav1.NewTime(ts),
				AgentIdentity:   agentID,
				AgentRequestRef: "test-ref",
				Event:           governancev1alpha1.AuditEventVerdictSubmitted,
				Action:          "test",
				TargetURI:       "k8s://cluster/test",
				Annotations:     map[string]string{"verdict": verdict},
			},
		}
		Expect(k8sClient.Create(ctx, ar)).To(Succeed())
	}

	// createCompletedRequest creates an AgentRequest labeled with the agent identity
	// and patches its status to Completed, so countTerminalExecutions counts it.
	createCompletedRequest := func(ns, agentID, name string) {
		ar := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				Labels: map[string]string{
					"aip.io/agentIdentity": agentID,
					"aip.io/profileName":   summaryNameForAgent(agentID),
				},
			},
			Spec: governancev1alpha1.AgentRequestSpec{
				AgentIdentity: agentID,
				Action:        "test",
				Reason:        "test execution",
				Target:        governancev1alpha1.Target{URI: "k8s://cluster/test"},
			},
		}
		Expect(k8sClient.Create(ctx, ar)).To(Succeed())
		base := ar.DeepCopy()
		ar.Status.Phase = governancev1alpha1.PhaseCompleted
		meta.SetStatusCondition(&ar.Status.Conditions, metav1.Condition{
			Type:   governancev1alpha1.ConditionExecuting,
			Status: metav1.ConditionTrue,
			Reason: "TestExecution",
		})
		Expect(k8sClient.Status().Patch(ctx, ar, client.MergeFrom(base))).To(Succeed())
	}

	// createProfileAt creates an AgentTrustProfile and patches its status to the
	// given trust level and lastPromotedAt so reconciliation starts from a known state.
	createProfileAt := func(ns, agentID, profileName, level string, lastPromotedAt time.Time) {
		profile := &governancev1alpha1.AgentTrustProfile{
			ObjectMeta: metav1.ObjectMeta{Name: profileName, Namespace: ns},
			Spec:       governancev1alpha1.AgentTrustProfileSpec{AgentIdentity: agentID},
		}
		Expect(k8sClient.Create(ctx, profile)).To(Succeed())
		base := profile.DeepCopy()
		lp := metav1.NewTime(lastPromotedAt)
		profile.Status.TrustLevel = level
		profile.Status.LastPromotedAt = &lp
		Expect(k8sClient.Status().Patch(ctx, profile, client.MergeFrom(base))).To(Succeed())
	}

	Context("AgentRegistration bootstrap", func() {
		var ns string

		BeforeEach(func() {
			ns = createNamespace("tp-areg")
		})

		It("bootstraps ATP when AgentRegistration is created before any AgentRequest", func() {
			agentID := "payment-bot"
			profileName := summaryNameForAgent(agentID)

			reg := &governancev1alpha1.AgentRegistration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "payment-bot-reg",
					Namespace: ns,
				},
				Spec: governancev1alpha1.AgentRegistrationSpec{
					AgentIdentity: agentID,
				},
			}
			Expect(k8sClient.Create(ctx, reg)).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, reg))).To(Succeed())
			})

			// Trigger reconcile manually (no real manager in envtest unit tests).
			r := newReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: profileName, Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())

			var atp governancev1alpha1.AgentTrustProfile
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)).To(Succeed())
			Expect(atp.Spec.AgentIdentity).To(Equal(agentID))

			// Reconcile again to verify idempotency (no error, no duplicate).
			_, err = r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: profileName, Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("uses AgentRegistration identity over DiagnosticAccuracySummary when both exist", func() {
			agentID := "reg-wins-agent"
			differentID := "das-agent-different"
			profileName := summaryNameForAgent(agentID)

			// Create a DAS with a different identity that maps to the same profile name.
			// (contrived, but verifies priority ordering.)
			das := &governancev1alpha1.DiagnosticAccuracySummary{
				ObjectMeta: metav1.ObjectMeta{Name: profileName, Namespace: ns},
				Spec:       governancev1alpha1.DiagnosticAccuracySummarySpec{AgentIdentity: differentID},
			}
			Expect(k8sClient.Create(ctx, das)).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, das))).To(Succeed())
			})

			reg := &governancev1alpha1.AgentRegistration{
				ObjectMeta: metav1.ObjectMeta{Name: "reg-wins", Namespace: ns},
				Spec:       governancev1alpha1.AgentRegistrationSpec{AgentIdentity: agentID},
			}
			Expect(k8sClient.Create(ctx, reg)).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, reg))).To(Succeed())
			})

			r := newReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: profileName, Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())

			var atp governancev1alpha1.AgentTrustProfile
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)).To(Succeed())
			// AgentRegistration identity wins.
			Expect(atp.Spec.AgentIdentity).To(Equal(agentID))
		})

		It("falls back to AgentRequest bootstrap when no AgentRegistration exists", func() {
			agentID := "legacy-agent-no-reg"
			profileName := summaryNameForAgent(agentID)

			ar := &governancev1alpha1.AgentRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ar-bootstrap",
					Namespace: ns,
					Labels:    map[string]string{"aip.io/profileName": profileName},
				},
				Spec: governancev1alpha1.AgentRequestSpec{
					AgentIdentity: agentID,
					Action:        "test",
					Reason:        "test",
					Target:        governancev1alpha1.Target{URI: "k8s://x"},
				},
			}
			Expect(k8sClient.Create(ctx, ar)).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ar))).To(Succeed())
			})

			r := newReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: profileName, Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())

			var atp governancev1alpha1.AgentTrustProfile
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)).To(Succeed())
			Expect(atp.Spec.AgentIdentity).To(Equal(agentID))
		})

		It("preserves ATP when AgentRegistration is deleted", func() {
			agentID := "preserved-agent"
			profileName := summaryNameForAgent(agentID)

			reg := &governancev1alpha1.AgentRegistration{
				ObjectMeta: metav1.ObjectMeta{Name: "to-delete-reg", Namespace: ns},
				Spec:       governancev1alpha1.AgentRegistrationSpec{AgentIdentity: agentID},
			}
			Expect(k8sClient.Create(ctx, reg)).To(Succeed())

			// Bootstrap the ATP.
			r := newReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: profileName, Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())

			var atp governancev1alpha1.AgentTrustProfile
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)).To(Succeed())

			// Delete Registration.
			Expect(k8sClient.Delete(ctx, reg)).To(Succeed())

			// ATP must still exist — verify it persists for several seconds, not just at
			// a single point in time (guards against a controller GC'ing it on reg delete).
			Consistently(func() error {
				var current governancev1alpha1.AgentTrustProfile
				return k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &current)
			}, 5*time.Second, 200*time.Millisecond).Should(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &atp))).To(Succeed())
			})
		})
	})

	It("should bootstrap AgentTrustProfile from DiagnosticAccuracySummary", func() {
		ns := createNamespace("tp-bootstrap")
		agentID := "agent-bootstrap"
		profileName := summaryNameForAgent(agentID)

		das := &governancev1alpha1.DiagnosticAccuracySummary{
			ObjectMeta: metav1.ObjectMeta{Name: profileName, Namespace: ns},
			Spec:       governancev1alpha1.DiagnosticAccuracySummarySpec{AgentIdentity: agentID},
		}
		Expect(k8sClient.Create(ctx, das)).To(Succeed())

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: profileName, Namespace: ns}}
		_, err := newReconciler().Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		var profile governancev1alpha1.AgentTrustProfile
		Expect(k8sClient.Get(ctx, req.NamespacedName, &profile)).To(Succeed())
		Expect(profile.Spec.AgentIdentity).To(Equal(agentID))
		Expect(profile.Status.TrustLevel).To(Equal(governancev1alpha1.TrustLevelObserver))
	})

	It("should bootstrap AgentTrustProfile from a terminal AgentRequest when no DAS exists", func() {
		ns := createNamespace("tp-ar-bootstrap")
		agentID := "agent-ar-bootstrap"
		profileName := summaryNameForAgent(agentID)

		ar := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-ar",
				Namespace: ns,
				Labels: map[string]string{
					"aip.io/agentIdentity": agentID,
					"aip.io/profileName":   profileName,
				},
			},
			Spec: governancev1alpha1.AgentRequestSpec{
				AgentIdentity: agentID,
				Action:        "test",
				Reason:        "bootstrap test",
				Target:        governancev1alpha1.Target{URI: "k8s://cluster/test"},
			},
		}
		Expect(k8sClient.Create(ctx, ar)).To(Succeed())
		base := ar.DeepCopy()
		ar.Status.Phase = governancev1alpha1.PhaseDenied
		Expect(k8sClient.Status().Patch(ctx, ar, client.MergeFrom(base))).To(Succeed())

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: profileName, Namespace: ns}}
		_, err := newReconciler().Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		var profile governancev1alpha1.AgentTrustProfile
		Expect(k8sClient.Get(ctx, req.NamespacedName, &profile)).To(Succeed())
		Expect(profile.Spec.AgentIdentity).To(Equal(agentID))
		Expect(profile.Status.TrustLevel).To(Equal(governancev1alpha1.TrustLevelObserver))
	})

	It("should be a no-op when neither DiagnosticAccuracySummary nor AgentRequest exists", func() {
		ns := createNamespace("tp-noop")
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "non-existent", Namespace: ns}}
		_, err := newReconciler().Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		var profile governancev1alpha1.AgentTrustProfile
		err = k8sClient.Get(ctx, req.NamespacedName, &profile)
		Expect(errors.IsNotFound(err)).To(BeTrue())
	})

	It("should promote from Trusted to Autonomous when thresholds are met", func() {
		ns := createNamespace("tp-promotion")
		agentID := "agent-promotion"
		profileName := summaryNameForAgent(agentID)

		// 24 hours before fixedTestTime — well outside the 1h grace period.
		createProfileAt(ns, agentID, profileName, governancev1alpha1.TrustLevelTrusted, fixedTestTime.Add(-24*time.Hour))

		// 5 correct verdicts → accuracy 1.0 (evaluation window = 5).
		for i := range 5 {
			createAuditRecord(ns, agentID, fmt.Sprintf("audit-%d", i), verdictCorrect,
				fixedTestTime.Add(time.Duration(i)*time.Second))
		}

		// 10 completed executions — meets Autonomous threshold (min=10).
		for i := range 10 {
			createCompletedRequest(ns, agentID, fmt.Sprintf("ar-%d", i))
		}

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: profileName, Namespace: ns}}
		_, err := newReconciler().Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		var p governancev1alpha1.AgentTrustProfile
		Expect(k8sClient.Get(ctx, req.NamespacedName, &p)).To(Succeed())
		Expect(p.Status.TrustLevel).To(Equal(governancev1alpha1.TrustLevelAutonomous))
		Expect(p.Status.LastPromotedAt).NotTo(BeNil())
	})

	It("should demote from Trusted to Supervised when accuracy falls below threshold", func() {
		ns := createNamespace("tp-demotion")
		agentID := "agent-demotion"
		profileName := summaryNameForAgent(agentID)

		// 24 hours ago — outside the 1h grace period so demotion is not blocked.
		createProfileAt(ns, agentID, profileName, governancev1alpha1.TrustLevelTrusted, fixedTestTime.Add(-24*time.Hour))

		// 4 correct + 1 incorrect = accuracy 0.8 (below Trusted min=0.9, above Supervised min=0.8).
		for i := range 4 {
			createAuditRecord(ns, agentID, fmt.Sprintf("audit-%d", i), verdictCorrect,
				fixedTestTime.Add(time.Duration(i)*time.Second))
		}
		createAuditRecord(ns, agentID, "audit-4", verdictIncorrect, fixedTestTime.Add(4*time.Second))

		// 5 completed executions — meets Supervised.min=2 and Trusted.min=5.
		for i := range 5 {
			createCompletedRequest(ns, agentID, fmt.Sprintf("ar-%d", i))
		}

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: profileName, Namespace: ns}}
		_, err := newReconciler().Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		// accuracy 0.8 < (Trusted.Min 0.9 - buffer 0.05 = 0.85) → checkDemotion=true → demote.
		// resolveTrustLevel(0.8, 5): Supervised (0.8 >= 0.8 && 5 >= 2) is the highest matching level.
		var p governancev1alpha1.AgentTrustProfile
		Expect(k8sClient.Get(ctx, req.NamespacedName, &p)).To(Succeed())
		Expect(p.Status.TrustLevel).To(Equal(governancev1alpha1.TrustLevelSupervised))
		Expect(p.Status.LastDemotedAt).NotTo(BeNil())
	})

	It("should hold level during demotion grace period", func() {
		ns := createNamespace("tp-grace")
		agentID := "agent-grace"
		profileName := summaryNameForAgent(agentID)

		// LastPromotedAt is 10 minutes ago — INSIDE the 1h grace period.
		createProfileAt(ns, agentID, profileName, governancev1alpha1.TrustLevelTrusted, fixedTestTime.Add(-10*time.Minute))

		// All incorrect — would normally demote all the way down.
		for i := range 5 {
			createAuditRecord(ns, agentID, fmt.Sprintf("audit-%d", i), verdictIncorrect,
				fixedTestTime.Add(time.Duration(i)*time.Second))
		}

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: profileName, Namespace: ns}}
		_, err := newReconciler().Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		// checkDemotion should return false (grace period active) → level held at Trusted.
		var p governancev1alpha1.AgentTrustProfile
		Expect(k8sClient.Get(ctx, req.NamespacedName, &p)).To(Succeed())
		Expect(p.Status.TrustLevel).To(Equal(governancev1alpha1.TrustLevelTrusted))
	})

	It("should route AgentRequest to AwaitingVerdict when AnnotationCanExecute is false", func() {
		ns := createNamespace("tp-routing")
		agentID := "agent-observer"

		ar := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-can-execute",
				Namespace: ns,
				Annotations: map[string]string{
					governancev1alpha1.AnnotationCanExecute: "false",
				},
			},
			Spec: governancev1alpha1.AgentRequestSpec{
				AgentIdentity: agentID,
				Action:        "test",
				Reason:        "trust gate routing test",
				Target:        governancev1alpha1.Target{URI: "k8s://cluster/test"},
			},
		}
		Expect(k8sClient.Create(ctx, ar)).To(Succeed())

		arReconciler := &AgentRequestReconciler{
			Client:          k8sClient,
			APIReader:       k8sClient,
			Scheme:          k8sClient.Scheme(),
			ApprovedTimeout: 5 * time.Minute,
		}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ar.Name, Namespace: ns}}
		_, err := arReconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		var fetched governancev1alpha1.AgentRequest
		Expect(k8sClient.Get(ctx, req.NamespacedName, &fetched)).To(Succeed())
		Expect(fetched.Status.Phase).To(Equal(governancev1alpha1.PhaseAwaitingVerdict))

		var found bool
		for _, c := range fetched.Status.Conditions {
			if c.Type == "RequestSubmitted" && c.Reason == "TrustGateBlock" {
				found = true
				break
			}
		}
		Expect(found).To(BeTrue(), "expected RequestSubmitted condition with Reason=TrustGateBlock")
	})

})
