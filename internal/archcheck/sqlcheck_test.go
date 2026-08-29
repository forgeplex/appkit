package archcheck_test

import (
	"testing"

	"github.com/forgeplex/appkit/internal/archcheck"
)

func TestCheckSQL(t *testing.T) {
	tests := []struct {
		name string
		sql  string // 写入 db/queries/q.sql
		want []wantV
	}{
		{
			name: "本域引用与列别名不误报",
			sql: `-- name: ListOutbox :many
SELECT o.id, o.topic, o.payload
FROM ledger.outbox o
WHERE o.status = 'new';
`,
			want: nil,
		},
		{
			name: "函数调用不误报",
			sql: `SELECT set_config('search_path', 'ledger', true);
SELECT pg_catalog.lower(name), audit.log_row(id)
FROM ledger.accounts
JOIN LATERAL jsonb_each(meta) kv ON true;
`,
			want: nil,
		},
		{
			name: "跨 schema JOIN 违规且行号准确",
			sql: `SELECT a.id
FROM ledger.accounts a
JOIN auth.users u ON u.id = a.owner_id;
`,
			want: []wantV{{File: "db/queries/q.sql", Line: 3, Msg: "跨 schema 引用 auth"}},
		},
		{
			name: "INSERT INTO 与 UPDATE 跨 schema 违规",
			sql: `INSERT INTO merchant.orders (id) VALUES ($1);
UPDATE gateway.routes SET active = false;
`,
			want: []wantV{
				{File: "db/queries/q.sql", Line: 1, Msg: "跨 schema 引用 merchant"},
				{File: "db/queries/q.sql", Line: 2, Msg: "跨 schema 引用 gateway"},
			},
		},
		{
			name: "REFERENCES 跨 schema 外键违规",
			sql: `CREATE TABLE ledger.entries (
    id uuid PRIMARY KEY,
    user_id uuid REFERENCES auth.users(id)
);
`,
			want: []wantV{{File: "db/queries/q.sql", Line: 3, Msg: "跨 schema 引用 auth"}},
		},
		{
			name: "IF EXISTS 经 EXISTS 捕获",
			sql: `DROP TABLE IF EXISTS ledger.tmp;
DROP TABLE IF EXISTS auth.tmp;
`,
			want: []wantV{{File: "db/queries/q.sql", Line: 2, Msg: "跨 schema 引用 auth"}},
		},
		{
			name: "系统 schema 放行",
			sql: `SELECT * FROM pg_catalog.pg_tables;
SELECT * FROM information_schema.columns;
SELECT * FROM public.schema_migrations;
`,
			want: nil,
		},
		{
			name: "大小写不敏感",
			sql:  "select * from AUTH.Users;\n",
			want: []wantV{{File: "db/queries/q.sql", Line: 1, Msg: "跨 schema 引用 auth"}},
		},
		{
			name: "带引号的限定名同样命中",
			sql:  `SELECT * FROM "auth"."users";` + "\n",
			want: []wantV{{File: "db/queries/q.sql", Line: 1, Msg: "跨 schema 引用 auth"}},
		},
		{
			name: "无 schema 限定不报",
			sql: `SELECT id FROM outbox WHERE status = 'new';
UPDATE outbox SET status = 'sent' WHERE id = $1;
`,
			want: nil,
		},
	}
	cfg := &archcheck.Config{Version: 1, Kind: archcheck.KindDomain, Domain: "ledger", Module: "github.com/forgeplex/ledger"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRepo(t, map[string]string{"db/queries/q.sql": tt.sql})
			got, err := archcheck.CheckSQL(dir, cfg)
			if err != nil {
				t.Fatalf("CheckSQL: %v", err)
			}
			assertViolations(t, got, tt.want)
		})
	}

	t.Run("db 目录不存在不报错", func(t *testing.T) {
		dir := writeRepo(t, map[string]string{})
		got, err := archcheck.CheckSQL(dir, cfg)
		if err != nil || len(got) != 0 {
			t.Fatalf("got %v, err %v", got, err)
		}
	})

	t.Run("migrations 目录下的 sql 一并扫描", func(t *testing.T) {
		dir := writeRepo(t, map[string]string{
			"db/migrations/0001_init.sql": "CREATE TABLE auth.users (id uuid);\n",
		})
		got, err := archcheck.CheckSQL(dir, cfg)
		if err != nil {
			t.Fatalf("CheckSQL: %v", err)
		}
		assertViolations(t, got, []wantV{{File: "db/migrations/0001_init.sql", Line: 1, Msg: "跨 schema 引用 auth"}})
	})
}
