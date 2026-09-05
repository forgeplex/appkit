package scaffold

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/forgeplex/appkit/ruleset"
)

// RenderDomain renders the entire scaffold without touching o.Dir. Migration
// DDL still comes from the framework library functions, never duplicated SQL.
// WorkflowRef must already be an explicit commit SHA: resolving provenance is
// the caller's responsibility and must not turn this renderer into external I/O.
func RenderDomain(o Options) (map[string][]byte, error) {
	if err := o.normalize(); err != nil {
		return nil, fmt.Errorf("new domain: %w", err)
	}
	if o.WorkflowRef == "" {
		return nil, fmt.Errorf("new domain: pure rendering requires an explicit WorkflowRef commit SHA")
	}
	files, err := renderFiles("domain", domainSpecs(o), newData(o, strings.ToUpper(o.Name)+"D"))
	if err != nil {
		return nil, fmt.Errorf("new domain %s: %w", o.Name, err)
	}
	files["db/migrations/0001_appkit_base.sql"] = []byte(baseMigrationSQL(o))
	if o.Tenant {
		files["db/migrations/0002_demo_notes.sql"] = []byte(tenantDemoSQL(o))
	}
	tool, err := SchemaToolFiles()
	if err != nil {
		return nil, err
	}
	for name, content := range tool {
		files[name] = content
	}
	cfg, err := ruleset.ParseAppConfig(files[".appkit.yml"])
	if err != nil {
		return nil, fmt.Errorf("new domain %s: 物化规则集: %w", o.Name, err)
	}
	rules, err := ruleset.Render(ruleset.Config{Domain: cfg.Domain, Module: cfg.Module, Contracts: cfg.Contracts,
		Version: o.AppkitVersion, WorkflowRef: o.WorkflowRef})
	if err != nil {
		return nil, fmt.Errorf("new domain %s: 物化规则集: %w", o.Name, err)
	}
	for name, content := range rules {
		files[name] = content
	}
	return files, nil
}

// RenderSystem renders the composition scaffold with no filesystem access.
func RenderSystem(o Options) (map[string][]byte, error) {
	if err := o.normalize(); err != nil {
		return nil, fmt.Errorf("new system: %w", err)
	}
	return renderFiles("system", systemFiles, newData(o, strings.ToUpper(o.Name)))
}

func domainSpecs(o Options) []fileSpec {
	switch {
	case o.Partitioned && o.Tenant:
		return swapTemplate(domainFiles, "module.go.tmpl", "module_partitioned_tenant.go.tmpl")
	case o.Partitioned:
		return swapTemplate(domainFiles, "module.go.tmpl", "module_partitioned.go.tmpl")
	case o.Tenant:
		return swapTemplate(domainFiles, "module.go.tmpl", "module_tenant.go.tmpl")
	default:
		return domainFiles
	}
}

func writeRenderedFiles(dir string, files map[string][]byte) error {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := writeFile(filepath.Join(dir, filepath.FromSlash(name)), files[name]); err != nil {
			return err
		}
	}
	return nil
}
