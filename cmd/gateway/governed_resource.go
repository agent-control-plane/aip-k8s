package main

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/gobwas/glob"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

const defaultGraduationPolicyName = "default"

var (
	globCache = make(map[string]glob.Glob)
	globMu    sync.RWMutex
)

// trustGateResult holds the outcome of a trust level evaluation.
type trustGateResult struct {
	rejected    bool
	message     string
	annotations map[string]string
}

// matchGovernedResource returns the most specific GovernedResource whose URIPattern
// matches targetURI using gobwas/glob semantics (** crosses separators, * does not).
// Most specific = longest pattern; ties broken alphabetically by name.
// Returns nil if no pattern matches.
func matchGovernedResource(items []v1alpha1.GovernedResource, targetURI string) *v1alpha1.GovernedResource {
	var best *v1alpha1.GovernedResource
	for i := range items {
		gr := &items[i]

		globMu.RLock()
		g, ok := globCache[gr.Spec.URIPattern]
		globMu.RUnlock()

		if !ok {
			var err error
			g, err = glob.Compile(gr.Spec.URIPattern, '/')
			if err != nil {
				log.Printf("invalid URIPattern %q in GovernedResource %s: %v", gr.Spec.URIPattern, gr.Name, err)
				continue
			}
			globMu.Lock()
			if len(globCache) > 1000 {
				globCache = make(map[string]glob.Glob)
			}
			globCache[gr.Spec.URIPattern] = g
			globMu.Unlock()
		}

		if !g.Match(targetURI) {
			continue
		}

		if best == nil ||
			len(gr.Spec.URIPattern) > len(best.Spec.URIPattern) ||
			(len(gr.Spec.URIPattern) == len(best.Spec.URIPattern) && gr.Name < best.Name) {
			best = gr
		}
	}
	return best
}

// evaluateTrustGate checks the agent's trust level against the GovernedResource's
// trust requirements. Returns a trustGateResult indicating whether the request
// should be rejected and what annotations to apply.
func (s *Server) evaluateTrustGate(
	ctx context.Context, ns, agentIdentity, mode string,
	gr *v1alpha1.GovernedResource,
) (trustGateResult, error) {
	tr := gr.Spec.TrustRequirements
	if tr == nil {
		return trustGateResult{}, nil
	}

	// Observer-mode requests bypass the trust floor check.
	if mode == v1alpha1.ModeObserve {
		return trustGateResult{}, nil
	}

	// Fetch the agent's trust profile. Use apiReader to bypass the informer cache —
	// a recently-deleted profile could otherwise be served stale, causing the wrong
	// canExecute annotation on the first request of a new demo run.
	profileName := v1alpha1.ProfileNameForAgent(agentIdentity)
	var profile v1alpha1.AgentTrustProfile
	if err := s.apiReader.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &profile); err != nil {
		if !apierrors.IsNotFound(err) {
			return trustGateResult{}, fmt.Errorf("getting AgentTrustProfile %s: %w", profileName, err)
		}
		// Profile not found — treat as Observer.
		profile.Status.TrustLevel = v1alpha1.TrustLevelObserver
	}

	effectiveLevel := profile.Status.TrustLevel
	if effectiveLevel == "" {
		effectiveLevel = v1alpha1.TrustLevelObserver
	}

	// Check minimum trust level.
	effRank, _ := v1alpha1.TrustLevelRank(effectiveLevel)
	if tr.MinTrustLevel != "" {
		minRank, minOk := v1alpha1.TrustLevelRank(tr.MinTrustLevel)
		if !minOk {
			return trustGateResult{
				rejected: true,
				message:  "GovernedResource has unknown MinTrustLevel: " + tr.MinTrustLevel,
			}, nil
		}
		if effRank < minRank {
			return trustGateResult{
				rejected: true,
				message:  "agent trust level " + effectiveLevel + " does not meet resource minimum " + tr.MinTrustLevel,
			}, nil
		}
	}

	// Compute effective autonomy = min(agent level, maxAutonomyLevel).
	effectiveAutonomy := effectiveLevel
	if maxRank, maxOk := v1alpha1.TrustLevelRank(tr.MaxAutonomyLevel); maxOk && effRank > maxRank {
		effectiveAutonomy = tr.MaxAutonomyLevel
	}

	// Fetch the graduation policy to determine if human approval is required.
	var policy v1alpha1.AgentGraduationPolicy
	requiresApproval := true // fail-closed default
	canExecute := false      // fail-closed default
	key := types.NamespacedName{Name: defaultGraduationPolicyName, Namespace: ns}
	if err := s.client.Get(ctx, key, &policy); err != nil {
		if !apierrors.IsNotFound(err) {
			return trustGateResult{}, fmt.Errorf("getting AgentGraduationPolicy default: %w", err)
		}
		// No policy found — use fail-closed defaults.
	} else {
		for _, level := range policy.Spec.Levels {
			if level.Name == effectiveAutonomy {
				canExecute = level.CanExecute
				requiresApproval = level.RequiresHumanApproval
				break
			}
		}
	}

	annotations := map[string]string{
		v1alpha1.AnnotationEffectiveTrustLevel: effectiveAutonomy,
	}
	if !canExecute {
		annotations[v1alpha1.AnnotationCanExecute] = boolToStr(false)
	} else {
		annotations[v1alpha1.AnnotationRequiresHumanApproval] = boolToStr(requiresApproval)
	}

	return trustGateResult{annotations: annotations}, nil
}

func boolToStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
