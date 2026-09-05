package agentplan

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/forgeplex/appkit/internal/gen"
	"github.com/forgeplex/appkit/internal/workspace"
)

// Events plans one file from a captured YAML input, without rereading its path.
func Events(ctx context.Context, root, input, target string) (workspace.Plan, error) {
	return single(ctx, root, input, target, func(data []byte) ([]byte, error) {
		return gen.RenderEventsSource(input, data)
	})
}

// Errors plans one error-code file from a captured YAML input.
func Errors(ctx context.Context, root, input, target string) (workspace.Plan, error) {
	return single(ctx, root, input, target, func(data []byte) ([]byte, error) {
		return gen.RenderErrorsSource(input, data)
	})
}

// Wrap selects an explicit interface source file rather than globbing a mutable
// directory. Other files in the package are not inputs to this plan; normal Go
// compilation still validates the whole package after apply.
func Wrap(ctx context.Context, root, input, target, iface, system string) (workspace.Plan, error) {
	if !strings.HasSuffix(input, ".go") || strings.HasSuffix(input, "_test.go") || path.Dir(input) != path.Dir(target) {
		return workspace.Plan{}, fmt.Errorf("%w: wrap requires a non-test .go input and output in the same package directory", workspace.ErrInvalidPath)
	}
	return single(ctx, root, input, target, func(data []byte) ([]byte, error) {
		return gen.RenderWrapSources(map[string][]byte{input: data}, iface, system)
	})
}

func single(ctx context.Context, root, input, target string, render func([]byte) ([]byte, error)) (workspace.Plan, error) {
	if !relative(input) || !relative(target) || !strings.HasSuffix(target, ".go") || strings.HasSuffix(target, "_test.go") {
		return workspace.Plan{}, fmt.Errorf("%w: input and output must be normalized workspace-relative paths; output must be a non-test .go file", workspace.ErrInvalidPath)
	}
	return build(ctx, root, input, func(data []byte) (map[string][]byte, error) {
		content, err := render(data)
		if err != nil {
			return nil, err
		}
		return map[string][]byte{target: content}, nil
	})
}
