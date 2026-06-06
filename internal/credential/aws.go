package credential

import (
	"context"
	"fmt"
)

// AWSWebIdentityProvider is a stub for the AWS AssumeRoleWithWebIdentity credential provider.
type AWSWebIdentityProvider struct{}

// NewAWSWebIdentityProvider creates a new AWSWebIdentityProvider.
func NewAWSWebIdentityProvider() *AWSWebIdentityProvider {
	return &AWSWebIdentityProvider{}
}

// Token returns an error as AWS credentials provider is not yet implemented.
func (p *AWSWebIdentityProvider) Token(ctx context.Context, rawOIDCToken string) (string, error) {
	return "", fmt.Errorf("AWSWebIdentityProvider is not implemented")
}
