package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	celtypes "github.com/google/cel-go/common/types"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel/model"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

func validateSafetypolicyCEL(
	_ context.Context,
	_ client.Client,
	policy *v1alpha1.SafetyPolicy,
	grList []v1alpha1.GovernedResource,
) error {
	// Always build a base environment with request/target so that CEL syntax
	// errors are caught at admission time even when no GovernedResource schema
	// is available yet.
	baseEnv, err := cel.NewEnv(
		cel.Variable("request", cel.DynType),
		cel.Variable("target", cel.DynType),
	)
	if err != nil {
		return fmt.Errorf("failed to create base CEL environment: %w", err)
	}

	// Try to find a matching GovernedResource with a schema so we can also
	// validate ctxData usage with full type information.
	env := baseEnv
	if policy.Spec.ContextType != "" {
		for _, gr := range grList {
			if gr.Spec.ContextFetcher == policy.Spec.ContextType && gr.Spec.ContextSchema != nil {
				var jsonSchemaV1 apiextensionsv1.JSONSchemaProps
				if err := json.Unmarshal(gr.Spec.ContextSchema.Raw, &jsonSchemaV1); err != nil {
					return fmt.Errorf("invalid contextSchema on GovernedResource %q: %w", gr.Name, err)
				}

				var jsonSchema apiextensions.JSONSchemaProps
				convertFn := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps
				if err := convertFn(&jsonSchemaV1, &jsonSchema, nil); err != nil {
					return fmt.Errorf("failed to convert contextSchema: %w", err)
				}

				structural, err := schema.NewStructural(&jsonSchema)
				if err != nil {
					return fmt.Errorf("failed to create structural schema from contextSchema: %w", err)
				}

				declType := model.SchemaDeclType(structural, false)
				if declType == nil {
					return fmt.Errorf("failed to create CEL declaration from contextSchema")
				}
				declType = declType.MaybeAssignTypeName("ContextType")

				typeProvider := apiservercel.NewDeclTypeProvider(declType)
				envOptions, err := typeProvider.EnvOptions(celtypes.NewEmptyRegistry())
				if err != nil {
					return fmt.Errorf("failed to get CEL env options: %w", err)
				}

				// Extended environment with ctxData typed from the schema.
				allOptions := append(envOptions,
					cel.Variable("request", cel.DynType),
					cel.Variable("target", cel.DynType),
					cel.Variable("ctxData", declType.CelType()),
				)
				extEnv, err := cel.NewEnv(allOptions...)
				if err != nil {
					return fmt.Errorf("failed to create CEL environment: %w", err)
				}
				env = extEnv
				break
			}
		}
	}

	var errs []string
	for _, rule := range policy.Spec.Rules {
		_, iss := env.Compile(rule.Expression)
		if iss.Err() != nil {
			errs = append(errs, fmt.Sprintf("rule %q: %v", rule.Name, iss.Err()))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("CEL type-check failed: %s", strings.Join(errs, "; "))
	}

	return nil
}
