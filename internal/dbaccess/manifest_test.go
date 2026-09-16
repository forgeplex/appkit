package dbaccess

import (
	"strings"
	"testing"
)

const validManifest = `version: 1
service: admin_api
roles:
  login:
    name: psp_admin_api
    managed: false
    attributes:
      login: true
      inherit: true
  permission:
    name: app_admin_api
    managed: true
    attributes:
      inherit: true
memberships:
  required: [app_admin_channel_test]
  forbidden: [app_admin]
grants:
  schemas:
    merchant: [USAGE]
  tables:
    merchant.admin_account: [SELECT, INSERT, UPDATE]
  sequences:
    merchant.admin_account_id_seq: [SELECT, USAGE]
  columns:
    - schema: merchant
      table: admin_account
      column: encrypted_key
      privileges: [SELECT]
  functions:
    - schema: merchant
      name: search_admin_account_keys
      arguments: [text, pg_catalog.uuid]
      privileges: [EXECUTE]
rls:
  merchant.admin_account:
    enabled: true
    forced: true
    requiredPolicies: [admin_account_tenant_isolation]
forbidden:
  roleAttributes: [SUPERUSER, BYPASSRLS, CREATEDB, CREATEROLE]
  privileges: [TRUNCATE, TRIGGER]
  mutations: [ledger.ledger_entry]
`

func TestParseValidManifest(t *testing.T) {
	m, err := Parse(strings.NewReader(validManifest))
	if err != nil {
		t.Fatal(err)
	}
	if m.Service != "admin_api" || m.Roles.Permission.Name != "app_admin_api" {
		t.Fatalf("unexpected manifest: %#v", m)
	}
}

func TestParseRejectsUnknownSecretAndMultipleDocuments(t *testing.T) {
	for name, source := range map[string]string{
		"unknown password":   strings.Replace(validManifest, "    managed: false", "    managed: false\n    password: secret", 1),
		"multiple documents": validManifest + "---\nversion: 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(source)); err == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
}

func TestParseRejectsUnsafeDeclarations(t *testing.T) {
	tests := map[string]string{
		"managed login":        strings.Replace(validManifest, "managed: false", "managed: true", 1),
		"superuser":            strings.Replace(validManifest, "      login: true", "      login: true\n      superuser: true", 1),
		"identifier injection": strings.Replace(validManifest, "app_admin_api", "app_admin_api;drop_role", 1),
		"unqualified table":    strings.Replace(validManifest, "merchant.admin_account: [SELECT, INSERT, UPDATE]", "admin_account: [SELECT]", 1),
		"unknown privilege":    strings.Replace(validManifest, "[SELECT, INSERT, UPDATE]", "[SELECT, OWN]", 1),
		"grant forbidden":      strings.Replace(validManifest, "privileges: [TRUNCATE, TRIGGER]", "privileges: [SELECT]", 1),
		"mutation conflict":    strings.Replace(validManifest, "mutations: [ledger.ledger_entry]", "mutations: [merchant.admin_account]", 1),
		"column mutation conflict": strings.Replace(strings.Replace(strings.Replace(validManifest,
			"merchant.admin_account: [SELECT, INSERT, UPDATE]", "merchant.admin_account: [SELECT]", 1),
			"      privileges: [SELECT]", "      privileges: [UPDATE]", 1),
			"mutations: [ledger.ledger_entry]", "mutations: [merchant.admin_account]", 1),
		"unqualified custom type":  strings.Replace(validManifest, "arguments: [text, pg_catalog.uuid]", "arguments: [custom_type]", 1),
		"duplicate column grant":   strings.Replace(validManifest, "  functions:\n", "    - schema: merchant\n      table: admin_account\n      column: encrypted_key\n      privileges: [UPDATE]\n  functions:\n", 1),
		"duplicate function grant": strings.Replace(validManifest, "rls:\n", "    - schema: merchant\n      name: search_admin_account_keys\n      arguments: [text, pg_catalog.uuid]\n      privileges: [EXECUTE]\nrls:\n", 1),
		"unqualified rls":          strings.Replace(validManifest, "rls:\n  merchant.admin_account:", "rls:\n  admin_account:", 1),
		"overqualified rls":        strings.Replace(validManifest, "rls:\n  merchant.admin_account:", "rls:\n  tenant.merchant.admin_account:", 1),
		"weak rls":                 strings.Replace(validManifest, "    forced: true", "    forced: false", 1),
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(source)); err == nil {
				t.Fatal("accepted unsafe manifest")
			}
		})
	}
}

func TestParseAllowsReadOnlyGrantOnForbiddenMutationTable(t *testing.T) {
	source := strings.Replace(validManifest,
		"    merchant.admin_account: [SELECT, INSERT, UPDATE]",
		"    merchant.admin_account: [SELECT, INSERT, UPDATE]\n    ledger.ledger_entry: [SELECT]", 1)
	if _, err := Parse(strings.NewReader(source)); err != nil {
		t.Fatalf("read-only access should coexist with forbidden mutations: %v", err)
	}
}

func TestParseAllowsQualifiedCustomFunctionType(t *testing.T) {
	source := strings.Replace(validManifest, "pg_catalog.uuid", "merchant.custom_type", 1)
	if _, err := Parse(strings.NewReader(source)); err != nil {
		t.Fatalf("qualified custom function type should be accepted: %v", err)
	}
}
