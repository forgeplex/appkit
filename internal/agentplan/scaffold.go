package agentplan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/forgeplex/appkit/internal/scaffold"
	"github.com/forgeplex/appkit/internal/workspace"
	"github.com/forgeplex/appkit/ruleset"
)

// New renders a domain/system scaffold into a new or empty target directory
// below an existing workspace. Plans only create files: existing generated-path
// contents cannot be overwritten even when equal. Inputs are explicit options
// and embedded framework templates, captured by the resulting plan payload.
func New(ctx context.Context, root, target, kind string, opts scaffold.Options) (plan workspace.Plan, err error) {
	if !(target == "." || relative(target)) {
		return workspace.Plan{}, fmt.Errorf("%w: target must be workspace-relative", workspace.ErrInvalidPath)
	}
	if kind != "domain" && kind != "system" {
		return workspace.Plan{}, fmt.Errorf("%w: new kind must be domain or system", workspace.ErrInvalidChange)
	}
	if kind == "system" && (opts.Tenant || opts.Partitioned) {
		return workspace.Plan{}, fmt.Errorf("%w: tenant/partitioned only apply to domains", workspace.ErrInvalidChange)
	}
	// Resolve before taking the workspace lock; pure renderers only consume the
	// pinned value, and apply never re-resolves it.
	if kind == "domain" {
		if opts.WorkflowRef == "" {
			opts.WorkflowRef, err = ruleset.ResolveWorkflowRefContext(ctx, opts.AppkitVersion)
		} else {
			opts.WorkflowRef, err = ruleset.NormalizeWorkflowRef(opts.WorkflowRef)
		}
		if err != nil {
			return workspace.Plan{}, err
		}
	}
	err = workspace.WithReadLock(ctx, root, func() error {
		// Probe through the same confined reader before inspecting target entries:
		// a symlink in target's ancestry must not escape the selected workspace.
		if _, _, err := workspace.ReadFile(root, path.Join(target, "go.mod"), workspace.MaxPlanContentBytes); err != nil {
			return err
		}
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(target)))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if len(entries) != 0 {
			return fmt.Errorf("%w: scaffold target %q must be absent or empty", workspace.ErrInvalidChange, target)
		}
		var files map[string][]byte
		if kind == "domain" {
			files, err = scaffold.RenderDomain(opts)
		} else {
			files, err = scaffold.RenderSystem(opts)
		}
		if err != nil {
			return err
		}
		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		sort.Strings(names)
		changes := make([]workspace.Change, 0, len(files))
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			changes = append(changes, workspace.Change{Path: path.Join(target, name), Operation: workspace.OperationCreate, Content: files[name], Mode: 0o644})
		}
		plan, err = workspace.BuildPlan(root, changes)
		return err
	})
	if err != nil {
		return workspace.Plan{}, err
	}
	return plan, nil
}
