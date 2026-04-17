package evaluation

import (
	"fmt"
	"sync"

	aipv1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"k8s.io/apimachinery/pkg/runtime"
)

type CELEnvironment struct {
	env      *cel.Env
	astCache map[string]*cel.Ast
	prgCache map[string]cel.Program
	mu       sync.RWMutex
}

// NewCELEnvironment initializes the CEL parsing environment with expected variables.
func NewCELEnvironment() (*CELEnvironment, error) {
	// `request` — the AgentRequest spec as declared by the agent.
	// `target`  — live cluster state fetched independently by the AIP control plane.
	//             Agents cannot influence `target`; it is authoritative cluster truth.
	env, err := cel.NewEnv(
		cel.Variable("request", cel.DynType),
		cel.Variable("target", cel.DynType),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL env: %w", err)
	}

	return &CELEnvironment{
		env:      env,
		astCache: make(map[string]*cel.Ast),
		prgCache: make(map[string]cel.Program),
	}, nil
}

// PrepareVariables transforms the AgentRequest and live TargetContext into a
// CEL variable map. Both `request` and `target` are available in expressions.
func (e *CELEnvironment) PrepareVariables(req *aipv1alpha1.AgentRequest, targetCtx *TargetContext) (map[string]any, error) {
	// Convert object to unstructured to get map[string]any representation
	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(req)
	if err != nil {
		return nil, err
	}

	var targetMap map[string]any
	if targetCtx != nil {
		targetMap = targetCtx.AsMap()
	} else {
		targetMap = (&TargetContext{}).AsMap()
	}

	return map[string]any{
		"request": unstructuredMap,
		"target":  targetMap,
	}, nil
}

// EvaluateExpression compiles and runs a CEL expression against parameters
func (e *CELEnvironment) EvaluateExpression(expr string, vars map[string]any) (bool, error) {
	e.mu.RLock()
	prg, ok := e.prgCache[expr]
	e.mu.RUnlock()

	if !ok {
		// Compile
		ast, iss := e.env.Compile(expr)
		if iss.Err() != nil {
			return false, fmt.Errorf("failed to compile CEL expression %q: %w", expr, iss.Err())
		}

		p, err := e.env.Program(ast)
		if err != nil {
			return false, fmt.Errorf("failed to instantiate CEL program %q: %w", expr, err)
		}

		e.mu.Lock()
		e.astCache[expr] = ast
		e.prgCache[expr] = p
		e.mu.Unlock()
		prg = p
	}

	val, _, err := prg.Eval(vars)
	if err != nil {
		return false, fmt.Errorf("evaluation error for %q: %w", expr, err)
	}

	// CEL expression from SafetyPolicy should return a boolean
	if types.IsError(val) {
		return false, fmt.Errorf("CEL result error: %v", val)
	}

	result, ok := val.Value().(bool)
	if !ok {
		return false, fmt.Errorf("CEL expression did not return a boolean, got: %T", val.Value())
	}

	return result, nil
}
