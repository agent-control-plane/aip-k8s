package evaluation

import "context"

// MockTargetContextFetcher returns pre-configured TargetContext values.
// Use this in tests and demos to simulate cluster state without a real cluster.
//
// Usage — single response for all URIs:
//
//	fetcher := &MockTargetContextFetcher{
//	    Default: &TargetContext{HasActiveEndpoints: true, ActiveEndpointCount: 3},
//	}
//
// Usage — per-URI responses:
//
//	fetcher := &MockTargetContextFetcher{
//	    Responses: map[string]*TargetContext{
//	        "k8s://prod/default/deployment/payment-api": {
//	            HasActiveEndpoints:  true,
//	            ActiveEndpointCount: 3,
//	            ReadyReplicas:       3,
//	            SpecReplicas:        3,
//	            DownstreamServices:  []string{"cost-explorer", "payment-worker"},
//	            Exists:              true,
//	        },
//	    },
//	}
type MockTargetContextFetcher struct {
	// Responses maps target URIs to their mock TargetContext.
	// Exact URI match is tried first.
	Responses map[string]*TargetContext
	// Default is returned when no URI-specific response is found.
	// If nil, returns an empty TargetContext (no active endpoints, no replicas).
	Default *TargetContext
	// FetchErr is returned as the error from Fetch when set.
	// Use to test error-handling paths.
	FetchErr error
}

func (m *MockTargetContextFetcher) Fetch(_ context.Context, targetURI string, _ string) (*TargetContext, error) {
	if m.FetchErr != nil {
		return nil, m.FetchErr
	}
	if m.Responses != nil {
		if ctx, ok := m.Responses[targetURI]; ok {
			return ctx, nil
		}
	}
	if m.Default != nil {
		return m.Default, nil
	}
	return &TargetContext{}, nil
}

// LiveTrafficContext returns a MockTargetContextFetcher pre-configured to simulate
// a deployment with live traffic — useful for the scale-down demo scenario.
func LiveTrafficContext(deploymentName string) *MockTargetContextFetcher {
	return &MockTargetContextFetcher{
		Default: &TargetContext{
			Exists:              true,
			HasActiveEndpoints:  true,
			ActiveEndpointCount: 3,
			ReadyReplicas:       3,
			SpecReplicas:        3,
			DownstreamServices:  []string{"cost-explorer", "payment-worker"},
			StateFingerprint:    "mock-v1",
		},
	}
}

// NoTrafficContext returns a MockTargetContextFetcher pre-configured to simulate
// a deployment with no active traffic — scale-down would be safe.
func NoTrafficContext() *MockTargetContextFetcher {
	return &MockTargetContextFetcher{
		Default: &TargetContext{
			Exists:              true,
			HasActiveEndpoints:  false,
			ActiveEndpointCount: 0,
			ReadyReplicas:       0,
			SpecReplicas:        3,
			DownstreamServices:  []string{},
			StateFingerprint:    "mock-v1",
		},
	}
}
