package agentplan

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/forgeplex/appkit/internal/schemadoc"
	"github.com/forgeplex/appkit/internal/workspace"
	"github.com/forgeplex/appkit/ruleset"
)

const schemaIndex = "db/SCHEMA.md"
const schemaOutput = "db/schema"
const schemaMigrations = "db/migrations"

// Schema executes trusted, captured migrations in a disposable database and
// plans their documentation. It never writes repository files. The caller must
// explicitly authorize scratch-database execution; migrations are not sandboxed
// SQL. Applying the resulting plan is entirely offline and filesystem-only.
func Schema(ctx context.Context, root, dsn string) (workspace.Plan, error) {
	return schemaPlan(ctx, root, dsn, schemadoc.Introspect)
}

func schemaPlan(ctx context.Context, root, dsn string, introspect func(context.Context, schemadoc.Options) (schemadoc.Schema, error)) (plan workspace.Plan, err error) {
	err = workspace.WithReadLock(ctx, root, func() error {
		states := map[string]workspace.File{}
		contents := map[string][]byte{}
		remaining := workspace.MaxPlanContentBytes
		read := func(name string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			data, state, err := workspace.ReadFile(root, name, int64(remaining))
			if err != nil {
				return err
			}
			remaining -= len(data)
			states[name], contents[name] = state, data
			return nil
		}
		if err := read(".appkit.yml"); err != nil {
			return err
		}
		if !states[".appkit.yml"].Exists {
			return fmt.Errorf("schema config: %w", fs.ErrNotExist)
		}
		cfg, err := ruleset.ParseAppConfig(contents[".appkit.yml"])
		if err != nil {
			return err
		}
		migrations, err := workspace.CaptureDirectory(root, schemaMigrations)
		if err != nil {
			return err
		}
		if !migrations.Exists {
			return fmt.Errorf("schema migrations: %w", fs.ErrNotExist)
		}
		outputs, err := workspace.CaptureDirectory(root, schemaOutput)
		if err != nil {
			return err
		}
		changes := []workspace.Change{{Path: ".appkit.yml", Operation: workspace.OperationAssert}}
		migrationFiles := map[string][]byte{}
		var migrationDirs []string
		for _, entry := range migrations.Entries {
			if entry.Kind != workspace.DirectoryFile {
				migrationDirs = append(migrationDirs, entry.Path)
				continue
			}
			name := path.Join(schemaMigrations, entry.Path)
			if err := read(name); err != nil {
				return err
			}
			if !states[name].Exists {
				return fmt.Errorf("%w: migration %s disappeared", workspace.ErrChanged, name)
			}
			migrationFiles[entry.Path] = contents[name]
			changes = append(changes, workspace.Change{Path: name, Operation: workspace.OperationAssert})
		}
		// Capture all existing output bytes before introspection. A renderer may
		// take seconds; a concurrent edit during that time is not an overwrite
		// that this plan was authorized to perform.
		oldOutputs := []string{schemaIndex}
		for _, entry := range outputs.Entries {
			if entry.Kind == workspace.DirectoryFile {
				oldOutputs = append(oldOutputs, path.Join(schemaOutput, entry.Path))
			}
		}
		for _, name := range oldOutputs {
			if err := read(name); err != nil {
				return err
			}
			if states[name].Exists && !generatedSchema(contents[name]) {
				return fmt.Errorf("%w: refusing to overwrite or prune non-generated schema file %q", workspace.ErrInvalidChange, name)
			}
		}
		model, err := introspect(ctx, schemadoc.Options{
			Dir: root, DSN: dsn, Schema: cfg.Domain, Partitioned: cfg.Partitioned,
			Migrations: newCapturedFS(migrationFiles, migrationDirs...),
		})
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		files, err := schemadoc.Render(model)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(files))
		for name := range files {
			if !relative(name) || (name != schemaIndex && !strings.HasPrefix(name, schemaOutput+"/")) {
				return fmt.Errorf("%w: schema renderer output %q", workspace.ErrInvalidPath, name)
			}
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			state, existed := states[name]
			if !existed {
				// The earlier directory capture proves this output was absent.
				state = workspace.File{Path: name}
				states[name] = state
			}
			change := workspace.Change{Path: name, Operation: workspace.OperationAssert}
			switch {
			case !state.Exists:
				change.Operation, change.Content, change.Mode = workspace.OperationCreate, []byte(files[name]), 0o644
			case !bytes.Equal(contents[name], []byte(files[name])):
				change.Operation, change.Content, change.Mode = workspace.OperationUpdate, []byte(files[name]), state.Mode
			}
			changes = append(changes, change)
		}
		for _, name := range oldOutputs {
			if _, retained := files[name]; !retained && states[name].Exists {
				changes = append(changes, workspace.Change{Path: name, Operation: workspace.OperationDelete})
			}
		}
		plan, err = workspace.BuildPlanWithGuards(root, changes, []workspace.DirectorySnapshot{migrations, outputs})
		if err != nil {
			return err
		}
		for _, state := range plan.Preconditions() {
			if state != states[state.Path] {
				return fmt.Errorf("%w: %s changed while planning", workspace.ErrChanged, state.Path)
			}
		}
		return nil
	})
	if err != nil {
		return workspace.Plan{}, err
	}
	return plan, nil
}

func generatedSchema(data []byte) bool {
	return bytes.HasPrefix(data, []byte("-- Code generated by appkit schema. DO NOT EDIT.\n")) ||
		bytes.HasPrefix(data, []byte("<!-- Code generated by appkit schema. DO NOT EDIT. -->\n"))
}
