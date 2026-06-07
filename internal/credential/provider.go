package credential

import (
	"context"
)

// Provider defines the interface for retrieving a bearer token for outbound requests.
type Provider interface {
	// Token returns the outbound credential token, potentially using the inbound OIDC raw token.
	Token(ctx context.Context, rawOIDCToken string) (string, error)
}
