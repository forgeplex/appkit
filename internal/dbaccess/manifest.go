// Package dbaccess parses and validates the declarative PostgreSQL access
// contract consumed by the appkit db-access command.
package dbaccess

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

const maxManifestSize = 1 << 20

var identifierRE = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

type Manifest struct {
	Version     int                       `yaml:"version"`
	Service     string                    `yaml:"service"`
	Roles       Roles                     `yaml:"roles"`
	Memberships Memberships               `yaml:"memberships,omitempty"`
	Grants      Grants                    `yaml:"grants,omitempty"`
	RLS         map[string]RLSRequirement `yaml:"rls,omitempty"`
	Forbidden   Forbidden                 `yaml:"forbidden,omitempty"`
}

type Roles struct {
	Login      Role `yaml:"login"`
	Permission Role `yaml:"permission"`
}

type Role struct {
	Name       string         `yaml:"name"`
	Managed    bool           `yaml:"managed"`
	Attributes RoleAttributes `yaml:"attributes,omitempty"`
}

type RoleAttributes struct {
	Login      bool `yaml:"login,omitempty"`
	Inherit    bool `yaml:"inherit,omitempty"`
	Superuser  bool `yaml:"superuser,omitempty"`
	BypassRLS  bool `yaml:"bypassRls,omitempty"`
	CreateDB   bool `yaml:"createDb,omitempty"`
	CreateRole bool `yaml:"createRole,omitempty"`
}

type Memberships struct {
	Required  []string `yaml:"required,omitempty"`
	Forbidden []string `yaml:"forbidden,omitempty"`
}

type Grants struct {
	Schemas   map[string][]string `yaml:"schemas,omitempty"`
	Tables    map[string][]string `yaml:"tables,omitempty"`
	Sequences map[string][]string `yaml:"sequences,omitempty"`
	Columns   []ColumnGrant       `yaml:"columns,omitempty"`
	Functions []FunctionGrant     `yaml:"functions,omitempty"`
}

type ColumnGrant struct {
	Schema     string   `yaml:"schema"`
	Table      string   `yaml:"table"`
	Column     string   `yaml:"column"`
	Privileges []string `yaml:"privileges"`
}

type FunctionGrant struct {
	Schema     string   `yaml:"schema"`
	Name       string   `yaml:"name"`
	Arguments  []string `yaml:"arguments,omitempty"`
	Privileges []string `yaml:"privileges"`
}

type RLSRequirement struct {
	Enabled          bool     `yaml:"enabled"`
	Forced           bool     `yaml:"forced"`
	RequiredPolicies []string `yaml:"requiredPolicies,omitempty"`
}

type Forbidden struct {
	RoleAttributes []string `yaml:"roleAttributes,omitempty"`
	Privileges     []string `yaml:"privileges,omitempty"`
	Mutations      []string `yaml:"mutations,omitempty"`
}

func LoadFile(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取数据库权限 manifest %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("读取数据库权限 manifest 状态 %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("数据库权限 manifest %s 不是普通文件", path)
	}
	if info.Size() > maxManifestSize {
		return nil, fmt.Errorf("数据库权限 manifest %s 超过 %d 字节上限", path, maxManifestSize)
	}
	return Parse(io.LimitReader(f, maxManifestSize+1))
}

func Parse(r io.Reader) (*Manifest, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("解析数据库权限 manifest: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("解析数据库权限 manifest: 只允许一个 YAML 文档")
		}
		return nil, fmt.Errorf("解析数据库权限 manifest 尾部: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m Manifest) Validate() error {
	var problems []string
	if m.Version != 1 {
		problems = append(problems, fmt.Sprintf("version 必须为 1（当前为 %d）", m.Version))
	}
	if err := validateIdentifier("service", m.Service); err != nil {
		problems = append(problems, err.Error())
	}
	if err := validateIdentifier("roles.login.name", m.Roles.Login.Name); err != nil {
		problems = append(problems, err.Error())
	}
	if err := validateIdentifier("roles.permission.name", m.Roles.Permission.Name); err != nil {
		problems = append(problems, err.Error())
	}
	if m.Roles.Login.Name != "" && m.Roles.Login.Name == m.Roles.Permission.Name {
		problems = append(problems, "login role 与 permission role 不得相同")
	}
	if m.Roles.Login.Managed {
		problems = append(problems, "roles.login.managed 必须为 false：登录账号及秘密由基础设施管理")
	}
	if !m.Roles.Login.Attributes.Login {
		problems = append(problems, "roles.login.attributes.login 必须为 true")
	}
	if !m.Roles.Login.Attributes.Inherit {
		problems = append(problems, "roles.login.attributes.inherit 必须为 true")
	}
	if !m.Roles.Permission.Managed {
		problems = append(problems, "roles.permission.managed 必须为 true")
	}
	if m.Roles.Permission.Attributes.Login {
		problems = append(problems, "roles.permission.attributes.login 必须为 false")
	}
	if !m.Roles.Permission.Attributes.Inherit {
		problems = append(problems, "roles.permission.attributes.inherit 必须为 true")
	}
	for field, value := range map[string]bool{
		"roles.login.attributes.superuser":       m.Roles.Login.Attributes.Superuser,
		"roles.login.attributes.bypassRls":       m.Roles.Login.Attributes.BypassRLS,
		"roles.login.attributes.createDb":        m.Roles.Login.Attributes.CreateDB,
		"roles.login.attributes.createRole":      m.Roles.Login.Attributes.CreateRole,
		"roles.permission.attributes.superuser":  m.Roles.Permission.Attributes.Superuser,
		"roles.permission.attributes.bypassRls":  m.Roles.Permission.Attributes.BypassRLS,
		"roles.permission.attributes.createDb":   m.Roles.Permission.Attributes.CreateDB,
		"roles.permission.attributes.createRole": m.Roles.Permission.Attributes.CreateRole,
	} {
		if value {
			problems = append(problems, field+" 不允许为 true")
		}
	}

	problems = append(problems, validateRoleLists(m.Memberships, m.Roles)...)
	problems = append(problems, validateGrantMap("grants.schemas", m.Grants.Schemas, schemaPrivileges, false)...)
	problems = append(problems, validateGrantMap("grants.tables", m.Grants.Tables, tablePrivileges, true)...)
	problems = append(problems, validateGrantMap("grants.sequences", m.Grants.Sequences, sequencePrivileges, true)...)
	for i, grant := range m.Grants.Columns {
		prefix := fmt.Sprintf("grants.columns[%d]", i)
		for field, value := range map[string]string{"schema": grant.Schema, "table": grant.Table, "column": grant.Column} {
			if err := validateIdentifier(prefix+"."+field, value); err != nil {
				problems = append(problems, err.Error())
			}
		}
		problems = append(problems, validatePrivileges(prefix+".privileges", grant.Privileges, columnPrivileges)...)
	}
	for i, grant := range m.Grants.Functions {
		prefix := fmt.Sprintf("grants.functions[%d]", i)
		if err := validateIdentifier(prefix+".schema", grant.Schema); err != nil {
			problems = append(problems, err.Error())
		}
		if err := validateIdentifier(prefix+".name", grant.Name); err != nil {
			problems = append(problems, err.Error())
		}
		for j, arg := range grant.Arguments {
			if err := validateTypeName(fmt.Sprintf("%s.arguments[%d]", prefix, j), arg); err != nil {
				problems = append(problems, err.Error())
			}
		}
		problems = append(problems, validatePrivileges(prefix+".privileges", grant.Privileges, functionPrivileges)...)
	}
	for table, required := range m.RLS {
		if err := validateQualifiedName("rls."+table, table); err != nil {
			problems = append(problems, err.Error())
		}
		if !required.Enabled || !required.Forced {
			problems = append(problems, "rls."+table+" 必须同时声明 enabled: true 与 forced: true")
		}
		problems = append(problems, validateIdentifiers("rls."+table+".requiredPolicies", required.RequiredPolicies)...)
	}
	problems = append(problems, validateEnumList("forbidden.roleAttributes", m.Forbidden.RoleAttributes, roleAttributeNames)...)
	problems = append(problems, validateEnumList("forbidden.privileges", m.Forbidden.Privileges, tablePrivileges)...)
	for i, name := range m.Forbidden.Mutations {
		if err := validateQualifiedName(fmt.Sprintf("forbidden.mutations[%d]", i), name); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return errors.New("数据库权限 manifest 无效：\n  " + strings.Join(problems, "\n  "))
	}
	return nil
}

var (
	schemaPrivileges   = []string{"CREATE", "USAGE"}
	tablePrivileges    = []string{"DELETE", "INSERT", "REFERENCES", "SELECT", "TRIGGER", "TRUNCATE", "UPDATE"}
	sequencePrivileges = []string{"SELECT", "UPDATE", "USAGE"}
	columnPrivileges   = []string{"INSERT", "REFERENCES", "SELECT", "UPDATE"}
	functionPrivileges = []string{"EXECUTE"}
	roleAttributeNames = []string{"BYPASSRLS", "CREATEDB", "CREATEROLE", "LOGIN", "SUPERUSER"}
)

func validateRoleLists(m Memberships, roles Roles) []string {
	var problems []string
	problems = append(problems, validateIdentifiers("memberships.required", m.Required)...)
	problems = append(problems, validateIdentifiers("memberships.forbidden", m.Forbidden)...)
	for _, name := range m.Required {
		if name == roles.Permission.Name {
			problems = append(problems, "permission role 会自动授予 login role，不得在 memberships.required 重复声明")
		}
		if name == roles.Login.Name {
			problems = append(problems, "login role 不得成为自己的 membership")
		}
		if slices.Contains(m.Forbidden, name) {
			problems = append(problems, fmt.Sprintf("membership %q 不能同时 required 与 forbidden", name))
		}
	}
	for _, name := range m.Forbidden {
		if name == roles.Permission.Name {
			problems = append(problems, "permission role 不得在 memberships.forbidden 中声明")
		}
		if name == roles.Login.Name {
			problems = append(problems, "login role 不得成为自己的 forbidden membership")
		}
	}
	return problems
}

func validateGrantMap(path string, grants map[string][]string, allowed []string, qualified bool) []string {
	var problems []string
	for name, privileges := range grants {
		if qualified {
			if err := validateQualifiedName(path+"."+name, name); err != nil {
				problems = append(problems, err.Error())
			}
		} else if err := validateIdentifier(path+"."+name, name); err != nil {
			problems = append(problems, err.Error())
		}
		problems = append(problems, validatePrivileges(path+"."+name, privileges, allowed)...)
	}
	return problems
}

func validatePrivileges(path string, privileges, allowed []string) []string {
	if len(privileges) == 0 {
		return []string{path + " 不得为空"}
	}
	return validateEnumList(path, privileges, allowed)
}

func validateEnumList(path string, values, allowed []string) []string {
	var problems []string
	seen := map[string]bool{}
	for _, value := range values {
		if !slices.Contains(allowed, value) {
			problems = append(problems, fmt.Sprintf("%s 包含不支持的值 %q（允许 %s）", path, value, strings.Join(allowed, ", ")))
		}
		if seen[value] {
			problems = append(problems, fmt.Sprintf("%s 重复声明 %q", path, value))
		}
		seen[value] = true
	}
	return problems
}

func validateIdentifiers(path string, values []string) []string {
	var problems []string
	seen := map[string]bool{}
	for i, value := range values {
		if err := validateIdentifier(fmt.Sprintf("%s[%d]", path, i), value); err != nil {
			problems = append(problems, err.Error())
		}
		if seen[value] {
			problems = append(problems, fmt.Sprintf("%s 重复声明 %q", path, value))
		}
		seen[value] = true
	}
	return problems
}

func validateIdentifier(path, value string) error {
	if !identifierRE.MatchString(value) || len(value) > 63 {
		return fmt.Errorf("%s=%q 不是合法的非引号 PostgreSQL 标识符", path, value)
	}
	return nil
}

func validateQualifiedName(path, value string) error {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return fmt.Errorf("%s=%q 必须是 schema.object", path, value)
	}
	for _, part := range parts {
		if err := validateIdentifier(path, part); err != nil {
			return err
		}
	}
	return nil
}

func validateTypeName(path, value string) error {
	base := strings.TrimSuffix(value, "[]")
	parts := strings.Split(base, ".")
	if len(parts) > 2 || (value != base && strings.HasSuffix(base, "[]")) {
		return fmt.Errorf("%s=%q 不是受支持的 PostgreSQL 类型名", path, value)
	}
	for _, part := range parts {
		if err := validateIdentifier(path, part); err != nil {
			return fmt.Errorf("%s=%q 不是受支持的 PostgreSQL 类型名", path, value)
		}
	}
	return nil
}

func normalizedYAML(m Manifest) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
