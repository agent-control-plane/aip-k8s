package main

import (
	"log"
	"sync"

	"github.com/gobwas/glob"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

var (
	globCache = make(map[string]glob.Glob)
	globMu    sync.RWMutex
)

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
