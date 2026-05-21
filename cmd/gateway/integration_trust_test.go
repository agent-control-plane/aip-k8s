package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const annotationFalse = "false"

// trustTestCleanup deletes all trust-gate resources created during a test run.
// It is safe to call even if resources do not exist.
func trustTestCleanup(ctx context.Context, c client.Client) {
	_ = c.DeleteAllOf(ctx, &v1alpha1.AgentRequest{}, client.InNamespace(testDefaultNS))
	_ = c.DeleteAllOf(ctx, &v1alpha1.AuditRecord{}, client.InNamespace(testDefaultNS))
	_ = c.DeleteAllOf(ctx, &v1alpha1.AgentTrustProfile{}, client.InNamespace(testDefaultNS))
	_ = c.DeleteAllOf(ctx, &v1alpha1.DiagnosticAccuracySummary{}, client.InNamespace(testDefaultNS))

	var grList v1alpha1.GovernedResourceList
	if err := c.List(ctx, &grList); err == nil {
		for i := range grList.Items {
			_ = c.Delete(ctx, &grList.Items[i])
		}
	}
	var policyList v1alpha1.AgentGraduationPolicyList
	if err := c.List(ctx, &policyList); err == nil {
		for i := range policyList.Items {
			_ = c.Delete(ctx, &policyList.Items[i])
		}
	}
}

// createTrustTestPolicy creates the shared AgentGraduationPolicy used by all trust gate tests.
// Observer and Advisor have CanExecute=false; Supervised, Trusted, Autonomous have CanExecute=true.
// Only Trusted and Autonomous have RequiresHumanApproval=false.
func createTrustTestPolicy(ctx context.Context, gm *gomega.WithT, c client.Client) *v1alpha1.AgentGraduationPolicy {
	pol := &v1alpha1.AgentGraduationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: testDefaultNS},
		Spec: v1alpha1.AgentGraduationPolicySpec{
			EvaluationWindow: v1alpha1.EvaluationWindow{Count: 5},
			DemotionPolicy:   v1alpha1.DemotionPolicy{WindowSize: 1},
			Levels: []v1alpha1.GraduationLevel{
				{Name: v1alpha1.TrustLevelObserver, CanExecute: false, RequiresHumanApproval: true},
				{
					Name: v1alpha1.TrustLevelAdvisor, CanExecute: false, RequiresHumanApproval: true,
					Accuracy: &v1alpha1.AccuracyBand{Min: ptr.To(0.7), DemotionBuffer: ptr.To(0.05)},
				},
				{
					Name: v1alpha1.TrustLevelSupervised, CanExecute: true, RequiresHumanApproval: true,
					Accuracy: &v1alpha1.AccuracyBand{Min: ptr.To(0.8), DemotionBuffer: ptr.To(0.05)},
				},
				{
					Name: v1alpha1.TrustLevelTrusted, CanExecute: true, RequiresHumanApproval: false,
					Accuracy: &v1alpha1.AccuracyBand{Min: ptr.To(0.9), DemotionBuffer: ptr.To(0.05)},
				},
				{
					Name: v1alpha1.TrustLevelAutonomous, CanExecute: true, RequiresHumanApproval: false,
					Accuracy:   &v1alpha1.AccuracyBand{Min: ptr.To(0.95), DemotionBuffer: ptr.To(0.02)},
					Executions: &v1alpha1.ExecutionBand{Min: ptr.To(int64(10))},
				},
			},
		},
	}
	gm.Expect(c.Create(ctx, pol)).To(gomega.Succeed())
	return pol
}

// createGovernedResourceWithTrust creates a cluster-scoped GovernedResource with a
// TrustRequirements block. uriPattern must match the test target URIs.
func createGovernedResourceWithTrust(
	ctx context.Context, gm *gomega.WithT, c client.Client,
	name, uriPattern, minLevel, maxAutonomy string,
) *v1alpha1.GovernedResource {
	gr := &v1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.GovernedResourceSpec{
			URIPattern:       uriPattern,
			PermittedActions: []string{"test"},
			ContextFetcher:   "none",
			TrustRequirements: &v1alpha1.TrustRequirements{
				MinTrustLevel:    minLevel,
				MaxAutonomyLevel: maxAutonomy,
			},
		},
	}
	gm.Expect(c.Create(ctx, gr)).To(gomega.Succeed())
	return gr
}

// profileLevelPrereqs returns the number of correct AuditRecords and completed AgentRequests
// needed for the reconciler to naturally compute the given trust level.
// Thresholds mirror createTrustTestPolicy; evaluation window is 5.
func profileLevelPrereqs(level string) (numVerdicts, numExecs int) {
	switch level {
	case v1alpha1.TrustLevelAutonomous:
		return 5, 10 // accuracy=1.0 ≥ 0.95, executions=10 ≥ 10
	case v1alpha1.TrustLevelTrusted:
		return 5, 5 // accuracy=1.0 ≥ 0.9, executions=5 ≥ 5
	case v1alpha1.TrustLevelSupervised:
		return 5, 2 // accuracy=1.0 ≥ 0.8, executions=2 ≥ 2
	case v1alpha1.TrustLevelAdvisor:
		return 5, 0 // accuracy=1.0 ≥ 0.7, no execution threshold
	default: // Observer
		return 0, 0
	}
}

// setProfileLevel seeds an agent's trust profile to the desired level by creating
// the AuditRecords and completed AgentRequests that cause the background reconciler
// to naturally compute that level.
//
// It waits for backing data to be visible in the informer cache (mgrClient) before
// creating the profile, ensuring the first reconcile computes the right level in one
// shot rather than converging via repeated rate-limited cycles.
func setProfileLevel(
	ctx context.Context, gm *gomega.WithT, mgrClient, directClient client.Client, agentID, level string,
) {
	profileName := v1alpha1.ProfileNameForAgent(agentID)
	numVerdicts, numExecs := profileLevelPrereqs(level)

	// Create AuditRecords so computeRollingAccuracy returns the required accuracy.
	for i := range numVerdicts {
		ar := &v1alpha1.AuditRecord{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-pv%d", profileName, i),
				Namespace: testDefaultNS,
				Labels:    map[string]string{"aip.io/agentIdentity": agentID},
			},
			Spec: v1alpha1.AuditRecordSpec{
				Timestamp:       metav1.Now(),
				AgentIdentity:   agentID,
				AgentRequestRef: "preset",
				Event:           v1alpha1.AuditEventVerdictSubmitted,
				Action:          "preset",
				TargetURI:       "k8s://preset/resource",
				Annotations:     map[string]string{"verdict": "correct"},
			},
		}
		gm.Expect(directClient.Create(ctx, ar)).To(gomega.Succeed())
	}

	// Create completed AgentRequests so countTerminalExecutions returns the required count.
	for i := range numExecs {
		req := &v1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-pe%d", profileName, i),
				Namespace: testDefaultNS,
				Labels:    map[string]string{"aip.io/agentIdentity": agentID},
			},
			Spec: v1alpha1.AgentRequestSpec{
				AgentIdentity: agentID,
				Action:        "preset",
				Reason:        "preset execution",
				Target:        v1alpha1.Target{URI: "k8s://preset/resource"},
			},
		}
		gm.Expect(directClient.Create(ctx, req)).To(gomega.Succeed())

		// Wait for the reconciler to assign an initial phase before we patch to
		// Completed. Without this, the reconciler races our Status patch and
		// overwrites Completed with its initial phase assignment.
		var latest v1alpha1.AgentRequest
		gm.Eventually(func() string {
			_ = directClient.Get(ctx, types.NamespacedName{Name: req.Name, Namespace: testDefaultNS}, &latest)
			return latest.Status.Phase
		}, eventuallyTimeout, eventuallyInterval).ShouldNot(gomega.BeEmpty())

		latest.Status.Phase = v1alpha1.PhaseCompleted
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:   v1alpha1.ConditionExecuting,
			Status: metav1.ConditionTrue,
			Reason: "TestExecution",
		})
		gm.Expect(directClient.Status().Update(ctx, &latest)).To(gomega.Succeed())
	}

	// Wait for all backing data to be visible in the informer cache. The reconciler uses
	// the cached client, so we must ensure data is there before triggering reconcile via
	// profile creation — otherwise the reconciler races the cache and converges slowly.
	if numExecs > 0 {
		gm.Eventually(func() int {
			var list v1alpha1.AgentRequestList
			_ = mgrClient.List(ctx, &list, client.InNamespace(testDefaultNS),
				client.MatchingLabels{"aip.io/agentIdentity": agentID})
			var count int
			for i := range list.Items {
				if list.Items[i].Status.Phase == v1alpha1.PhaseCompleted &&
					list.Items[i].Spec.Action == "preset" {
					count++
				}
			}
			return count
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(numExecs),
			"preset completed AgentRequests for %s not yet in cache", agentID)
	}
	if numVerdicts > 0 {
		gm.Eventually(func() int {
			var list v1alpha1.AuditRecordList
			_ = mgrClient.List(ctx, &list, client.InNamespace(testDefaultNS),
				client.MatchingLabels{"aip.io/agentIdentity": agentID})
			var count int
			for i := range list.Items {
				if list.Items[i].Spec.Event == v1alpha1.AuditEventVerdictSubmitted {
					count++
				}
			}
			return count
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(numVerdicts),
			"preset AuditRecords for %s not yet in cache", agentID)
	}

	// Create the profile. Now that all backing data is in the cache, the first reconcile
	// (triggered by profile creation via For(&AgentTrustProfile{})) will compute the
	// correct level immediately.
	profile := &v1alpha1.AgentTrustProfile{}
	profileKey := types.NamespacedName{Name: profileName, Namespace: testDefaultNS}
	if err := directClient.Get(ctx, profileKey, profile); err != nil {
		profile = &v1alpha1.AgentTrustProfile{
			ObjectMeta: metav1.ObjectMeta{Name: profileName, Namespace: testDefaultNS},
			Spec:       v1alpha1.AgentTrustProfileSpec{AgentIdentity: agentID},
		}
		gm.Expect(directClient.Create(ctx, profile)).To(gomega.Succeed())
	}

	// Wait for the reconciler to confirm the target level.
	gm.Eventually(func() string {
		var p v1alpha1.AgentTrustProfile
		_ = directClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: testDefaultNS}, &p)
		return p.Status.TrustLevel
	}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(level))
}

// submitVerdict POSTs a verdict for the given AgentRequest name and asserts success.
func submitVerdict(ctx context.Context, gm *gomega.WithT, s *Server, name, verdict string) {
	body := map[string]string{
		"verdict":   verdict,
		"verdictBy": testReviewerSub,
	}
	jsonBody, _ := json.Marshal(body)
	req := httptest.NewRequest("PATCH", "/agent-requests/"+name+"/verdict", bytes.NewBuffer(jsonBody))
	req = req.WithContext(withCallerSub(ctx, testReviewerSub))
	req.SetPathValue("name", name)
	rr := httptest.NewRecorder()
	s.handleVerdictAgentRequest(rr, req)
	gm.Expect(rr.Code).To(gomega.Equal(http.StatusOK), "verdict submission failed: %s", rr.Body.String())
}

// postAgentRequest sends a single POST /agent-requests and returns the recorder.
// The call is synchronous; the gateway polls internally until a phase is reached or
// waitTimeout fires. For AwaitingVerdict/Approved the gateway returns immediately.
func postAgentRequest(ctx context.Context, s *Server, agentID, targetURI string) *httptest.ResponseRecorder {
	body := createAgentRequestBody{
		AgentIdentity: agentID,
		Action:        "test",
		TargetURI:     targetURI,
		Reason:        "trust gate integration test",
		Namespace:     testDefaultNS,
	}
	jsonBody, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
	req = req.WithContext(withCallerSub(ctx, agentID))
	rr := httptest.NewRecorder()
	s.handleCreateAgentRequest(rr, req)
	return rr
}

func runTrustGateTests(t *testing.T, mgrClient, directClient client.Client, ctx context.Context) {
	t.Run("MinTrustLevel: agent below minimum is rejected with 403", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		defer trustTestCleanup(ctx, directClient)

		const agentID = "trust-min-agent"
		const targetURI = "k8s://trust-min/resource"

		pol := createTrustTestPolicy(ctx, gm, directClient)
		defer func() { _ = directClient.Delete(ctx, pol) }()

		gr := createGovernedResourceWithTrust(ctx, gm, directClient,
			"trust-min-gr", "k8s://trust-min/**",
			v1alpha1.TrustLevelSupervised, // minimum = Supervised
			v1alpha1.TrustLevelAutonomous,
		)
		defer func() { _ = directClient.Delete(ctx, gr) }()

		gm.Eventually(func() error {
			return mgrClient.Get(ctx, types.NamespacedName{Name: gr.Name}, &v1alpha1.GovernedResource{})
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Succeed())

		setProfileLevel(ctx, gm, mgrClient, directClient, agentID, v1alpha1.TrustLevelObserver)

		s := &Server{
			client:      directClient,
			apiReader:   directClient,
			dedupWindow: 0,
			waitTimeout: serverWaitTimeout,
			roles:       newRoleConfig("", "", "", "", "", ""),
		}

		rr := postAgentRequest(ctx, s, agentID, targetURI)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusForbidden))
		gm.Expect(rr.Body.String()).To(gomega.ContainSubstring("INSUFFICIENT_TRUST"))
	})

	t.Run("Observer: request routes to AwaitingVerdict via AnnotationCanExecute=false", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		defer trustTestCleanup(ctx, directClient)

		const agentID = "trust-observer-agent"
		const targetURI = "k8s://trust-obs/resource"

		pol := createTrustTestPolicy(ctx, gm, directClient)
		defer func() { _ = directClient.Delete(ctx, pol) }()

		gr := createGovernedResourceWithTrust(ctx, gm, directClient,
			"trust-obs-gr", "k8s://trust-obs/**",
			v1alpha1.TrustLevelObserver,
			v1alpha1.TrustLevelAutonomous,
		)
		defer func() { _ = directClient.Delete(ctx, gr) }()

		gm.Eventually(func() error {
			return mgrClient.Get(ctx, types.NamespacedName{Name: gr.Name}, &v1alpha1.GovernedResource{})
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Succeed())

		setProfileLevel(ctx, gm, mgrClient, directClient, agentID, v1alpha1.TrustLevelObserver)

		s := &Server{
			client:      directClient,
			apiReader:   directClient,
			dedupWindow: 0,
			waitTimeout: serverWaitTimeout,
			roles:       newRoleConfig("", "", "", "", "", ""),
		}

		respCh := make(chan *httptest.ResponseRecorder, 1)
		go func() { respCh <- postAgentRequest(ctx, s, agentID, targetURI) }()

		var rr *httptest.ResponseRecorder
		gm.Eventually(respCh, eventuallyLongTimeout).Should(gomega.Receive(&rr))
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusCreated))

		var respBody map[string]any
		gm.Expect(json.Unmarshal(rr.Body.Bytes(), &respBody)).To(gomega.Succeed())
		gm.Expect(respBody["phase"]).To(gomega.Equal(v1alpha1.PhaseAwaitingVerdict))

		// Verify at least one real AR (action=test) has AnnotationCanExecute=false.
		var arList v1alpha1.AgentRequestList
		gm.Expect(directClient.List(ctx, &arList, client.InNamespace(testDefaultNS),
			client.MatchingLabels{"aip.io/agentIdentity": agentID})).To(gomega.Succeed())
		var found bool
		for i := range arList.Items {
			if arList.Items[i].Spec.Action == "test" &&
				arList.Items[i].Annotations[v1alpha1.AnnotationCanExecute] == annotationFalse {
				found = true
				break
			}
		}
		gm.Expect(found).To(gomega.BeTrue(), "expected a 'test' AR with AnnotationCanExecute=false")
	})

	t.Run("MaxAutonomyLevel: Autonomous agent is capped at Supervised-level annotations", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		defer trustTestCleanup(ctx, directClient)

		const agentID = "trust-cap-agent"
		const targetURI = "k8s://trust-cap/resource"

		pol := createTrustTestPolicy(ctx, gm, directClient)
		defer func() { _ = directClient.Delete(ctx, pol) }()

		// MaxAutonomyLevel = Supervised caps an Autonomous-level agent.
		gr := createGovernedResourceWithTrust(ctx, gm, directClient,
			"trust-cap-gr", "k8s://trust-cap/**",
			v1alpha1.TrustLevelObserver,
			v1alpha1.TrustLevelSupervised, // ceiling
		)
		defer func() { _ = directClient.Delete(ctx, gr) }()

		gm.Eventually(func() error {
			return mgrClient.Get(ctx, types.NamespacedName{Name: gr.Name}, &v1alpha1.GovernedResource{})
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Succeed())

		// Seed Autonomous: 5 correct verdicts + 10 completed executions.
		// Wait for the informer cache to reflect all data before creating the profile so
		// the first reconcile computes Autonomous directly.
		setProfileLevel(ctx, gm, mgrClient, directClient, agentID, v1alpha1.TrustLevelAutonomous)

		s := &Server{
			client:      directClient,
			apiReader:   directClient,
			dedupWindow: 0,
			waitTimeout: serverWaitTimeout,
			roles:       newRoleConfig("", "", "", "", "", ""),
		}

		respCh := make(chan *httptest.ResponseRecorder, 1)
		go func() { respCh <- postAgentRequest(ctx, s, agentID, targetURI) }()

		var rr *httptest.ResponseRecorder
		gm.Eventually(respCh, eventuallyLongTimeout).Should(gomega.Receive(&rr))
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusCreated))

		// Find the real request (action=test) — preset ARs have action=preset.
		var arList v1alpha1.AgentRequestList
		gm.Expect(directClient.List(ctx, &arList, client.InNamespace(testDefaultNS),
			client.MatchingLabels{"aip.io/agentIdentity": agentID})).To(gomega.Succeed())

		var ar *v1alpha1.AgentRequest
		for i := range arList.Items {
			if arList.Items[i].Spec.Action == "test" {
				ar = &arList.Items[i]
				break
			}
		}
		gm.Expect(ar).NotTo(gomega.BeNil(), "expected a 'test' AgentRequest from postAgentRequest")
		// Effective level = Supervised (capped from Autonomous).
		gm.Expect(ar.Annotations[v1alpha1.AnnotationEffectiveTrustLevel]).To(gomega.Equal(v1alpha1.TrustLevelSupervised))
		// Supervised.CanExecute=true → annotation is NOT "false".
		gm.Expect(ar.Annotations[v1alpha1.AnnotationCanExecute]).NotTo(gomega.Equal(annotationFalse))
		// Supervised.RequiresHumanApproval=true.
		gm.Expect(ar.Annotations[v1alpha1.AnnotationRequiresHumanApproval]).To(gomega.Equal("true"))
	})

	t.Run("Trusted agent with RequiresHumanApproval=false is auto-approved", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		defer trustTestCleanup(ctx, directClient)

		const agentID = "trust-auto-agent"
		const targetURI = "k8s://trust-auto/resource"

		pol := createTrustTestPolicy(ctx, gm, directClient)
		defer func() { _ = directClient.Delete(ctx, pol) }()

		gr := createGovernedResourceWithTrust(ctx, gm, directClient,
			"trust-auto-gr", "k8s://trust-auto/**",
			v1alpha1.TrustLevelObserver,
			v1alpha1.TrustLevelAutonomous,
		)
		defer func() { _ = directClient.Delete(ctx, gr) }()

		// SafetyPolicy demands human approval; trust gate should bypass it for Trusted agents.
		approvalPolicy := createApprovalPolicy(ctx, gm, directClient, "trust-auto-approval", targetURI)
		defer func() { _ = directClient.Delete(ctx, approvalPolicy) }()

		gm.Eventually(func() error {
			return mgrClient.Get(ctx, types.NamespacedName{Name: gr.Name}, &v1alpha1.GovernedResource{})
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Succeed())

		// Seed Trusted: 5 correct verdicts + 5 completed executions.
		setProfileLevel(ctx, gm, mgrClient, directClient, agentID, v1alpha1.TrustLevelTrusted)

		s := &Server{
			client:      directClient,
			apiReader:   directClient,
			dedupWindow: 0,
			waitTimeout: serverWaitTimeout,
			roles:       newRoleConfig("", "", "", "", "", ""),
		}

		// Trusted: CanExecute=true, RequiresHumanApproval=false.
		// SafetyPolicy returns RequireApproval, but annotation overrides → auto-approved.
		respCh := make(chan *httptest.ResponseRecorder, 1)
		go func() { respCh <- postAgentRequest(ctx, s, agentID, targetURI) }()

		var rr *httptest.ResponseRecorder
		gm.Eventually(respCh, eventuallyLongTimeout).Should(gomega.Receive(&rr))
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusCreated))

		var respBody map[string]any
		gm.Expect(json.Unmarshal(rr.Body.Bytes(), &respBody)).To(gomega.Succeed())
		gm.Expect(respBody["phase"]).To(gomega.Equal(v1alpha1.PhaseApproved))
	})

	t.Run("Graduation ladder: Observer graduates to Trusted after 5 correct verdicts", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		defer trustTestCleanup(ctx, directClient)

		const agentID = "trust-grad-agent"
		const targetURIPattern = "k8s://trust-grad/**"

		pol := createTrustTestPolicy(ctx, gm, directClient)
		defer func() { _ = directClient.Delete(ctx, pol) }()

		// SafetyPolicy requiring approval so we can verify Trusted agents are auto-approved.
		const gradTargetURI = "k8s://trust-grad/resource"
		approvalPolicy := createApprovalPolicy(ctx, gm, directClient, "trust-grad-approval", gradTargetURI)
		defer func() { _ = directClient.Delete(ctx, approvalPolicy) }()

		gr := createGovernedResourceWithTrust(ctx, gm, directClient,
			"trust-grad-gr", targetURIPattern,
			v1alpha1.TrustLevelObserver,
			v1alpha1.TrustLevelAutonomous,
		)
		defer func() { _ = directClient.Delete(ctx, gr) }()

		// Start agent at Observer — no backing data needed; reconciler computes Observer
		// with accuracy=0, executions=0.
		setProfileLevel(ctx, gm, mgrClient, directClient, agentID, v1alpha1.TrustLevelObserver)

		gm.Eventually(func() error {
			return mgrClient.Get(ctx, types.NamespacedName{Name: gr.Name}, &v1alpha1.GovernedResource{})
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Succeed())

		s := &Server{
			client:      directClient,
			apiReader:   directClient,
			dedupWindow: 0,
			waitTimeout: serverWaitTimeout,
			roles:       newRoleConfig("", "", "", "", "", ""),
		}

		// Phase 1: Submit 5 requests as Observer — all land in AwaitingVerdict.
		const numRequests = 5
		arNames := make([]string, 0, numRequests)
		for i := range numRequests {
			uniqueURI := fmt.Sprintf("k8s://trust-grad/resource-%d", i)
			respCh := make(chan *httptest.ResponseRecorder, 1)
			go func() { respCh <- postAgentRequest(ctx, s, agentID, uniqueURI) }()

			var rr *httptest.ResponseRecorder
			gm.Eventually(respCh, eventuallyLongTimeout).Should(gomega.Receive(&rr))
			gm.Expect(rr.Code).To(gomega.Equal(http.StatusCreated))

			var body map[string]any
			gm.Expect(json.Unmarshal(rr.Body.Bytes(), &body)).To(gomega.Succeed())
			gm.Expect(body["phase"]).To(gomega.Equal(v1alpha1.PhaseAwaitingVerdict),
				"Observer request %d should land in AwaitingVerdict", i)
			arNames = append(arNames, body["name"].(string))
		}

		// Phase 2: Submit correct verdicts for all 5 requests.
		verdictServer := &Server{
			client:      directClient,
			apiReader:   directClient,
			dedupWindow: 0,
			waitTimeout: serverWaitTimeout,
			roles:       newRoleConfig("", "", "", "", "", ""),
		}
		for _, name := range arNames {
			gm.Eventually(func() string {
				var ar v1alpha1.AgentRequest
				_ = directClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testDefaultNS}, &ar)
				return ar.Status.Phase
			}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseAwaitingVerdict))

			submitVerdict(ctx, gm, verdictServer, name, "correct")
		}

		// Phase 3: Wait for all 5 requests to reach Completed.
		for _, name := range arNames {
			gm.Eventually(func() string {
				var ar v1alpha1.AgentRequest
				_ = directClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testDefaultNS}, &ar)
				return ar.Status.Phase
			}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseCompleted))
		}

		// Phase 4: Wait for AgentTrustProfile to be promoted.
		// 5 correct verdicts (accuracy=1.0) + 5 completed executions → Trusted.
		profileName := v1alpha1.ProfileNameForAgent(agentID)
		gm.Eventually(func() string {
			var profile v1alpha1.AgentTrustProfile
			_ = directClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: testDefaultNS}, &profile)
			return profile.Status.TrustLevel
		}, 30*time.Second, eventuallyInterval).Should(gomega.Equal(v1alpha1.TrustLevelTrusted))

		// Phase 5: Submit a 6th request as Trusted agent — should be auto-approved.
		// SafetyPolicy demands approval, but RequiresHumanApproval=false for Trusted
		// means the controller skips the human gate.
		respCh := make(chan *httptest.ResponseRecorder, 1)
		go func() { respCh <- postAgentRequest(ctx, s, agentID, gradTargetURI) }()

		var rr *httptest.ResponseRecorder
		gm.Eventually(respCh, eventuallyLongTimeout).Should(gomega.Receive(&rr))
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusCreated))

		var body map[string]any
		gm.Expect(json.Unmarshal(rr.Body.Bytes(), &body)).To(gomega.Succeed())
		gm.Expect(body["phase"]).To(gomega.Equal(v1alpha1.PhaseApproved),
			"Trusted agent should be auto-approved after graduation")
	})
}

func runTrustProfileReadTests(t *testing.T, _, directClient client.Client, ctx context.Context) {
	t.Run("GET /agent-trust-profiles lists profiles", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		defer func() {
			_ = directClient.DeleteAllOf(ctx, &v1alpha1.AgentTrustProfile{}, client.InNamespace(testDefaultNS))
		}()

		profile := &v1alpha1.AgentTrustProfile{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-profile-1",
				Namespace: testDefaultNS,
			},
			Spec: v1alpha1.AgentTrustProfileSpec{
				AgentIdentity: "agent-tp-1",
			},
		}
		gm.Expect(directClient.Create(ctx, profile)).To(gomega.Succeed())

		// Patch status separately to avoid validation webhook issues
		orig := profile.DeepCopy()
		profile.Status.TrustLevel = v1alpha1.TrustLevelAdvisor
		gm.Expect(directClient.Status().Patch(ctx, profile, client.MergeFrom(orig))).To(gomega.Succeed())

		s := &Server{
			client:       directClient,
			apiReader:    directClient,
			dedupWindow:  0,
			waitTimeout:  serverWaitTimeout,
			roles:        newRoleConfig("agent-tp-1", "reviewer-tp", "admin-tp", "", "", ""),
			authRequired: true,
		}

		req := httptest.NewRequest("GET", "/agent-trust-profiles?namespace="+testDefaultNS, nil)
		req = req.WithContext(withCallerSub(req.Context(), "admin-tp"))
		rr := httptest.NewRecorder()

		s.handleListAgentTrustProfiles(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	})

	t.Run("GET /agent-trust-profiles/{name} returns profile", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		defer func() {
			_ = directClient.DeleteAllOf(ctx, &v1alpha1.AgentTrustProfile{}, client.InNamespace(testDefaultNS))
		}()

		profile := &v1alpha1.AgentTrustProfile{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-profile-2",
				Namespace: testDefaultNS,
			},
			Spec: v1alpha1.AgentTrustProfileSpec{
				AgentIdentity: "agent-tp-2",
			},
		}
		gm.Expect(directClient.Create(ctx, profile)).To(gomega.Succeed())

		orig := profile.DeepCopy()
		profile.Status.TrustLevel = v1alpha1.TrustLevelTrusted
		gm.Expect(directClient.Status().Patch(ctx, profile, client.MergeFrom(orig))).To(gomega.Succeed())

		s := &Server{
			client:       directClient,
			apiReader:    directClient,
			dedupWindow:  0,
			waitTimeout:  serverWaitTimeout,
			roles:        newRoleConfig("agent-tp-2", "reviewer-tp", "admin-tp", "", "", ""),
			authRequired: true,
		}

		req := httptest.NewRequest("GET", "/agent-trust-profiles/test-profile-2?namespace="+testDefaultNS, nil)
		req = req.WithContext(withCallerSub(req.Context(), "admin-tp"))
		req.SetPathValue("name", "test-profile-2")
		rr := httptest.NewRecorder()

		s.handleGetAgentTrustProfile(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	})
}
