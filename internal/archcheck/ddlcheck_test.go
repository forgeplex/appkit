package archcheck_test

import (
	"testing"

	"github.com/forgeplex/appkit/internal/archcheck"
)

func TestCheckMigrationDDL(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want []wantV
	}{
		{
			name: "安全的建表/加默认列/函数体内语句不误报",
			sql: `CREATE TABLE ledger.accounts (
	id text PRIMARY KEY,
	balance numeric NOT NULL
);
ALTER TABLE ledger.accounts ADD COLUMN status text NOT NULL DEFAULT 'active';
ALTER TABLE ledger.accounts ADD COLUMN note text;
ALTER TABLE ledger.accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.accounts ADD CONSTRAINT chk_status CHECK (status <> '') NOT VALID;
CREATE OR REPLACE FUNCTION ledger.noop() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
	-- DROP TABLE inside function body is ignored
	PERFORM 1;
END;
$$;
`,
		},
		{
			name: "ADD COLUMN NOT NULL 无 DEFAULT 报错",
			sql:  "ALTER TABLE ledger.accounts ADD COLUMN currency text NOT NULL;\n",
			want: []wantV{{File: "db/migrations/0002_bad.sql", Line: 1, Msg: "ADD COLUMN NOT NULL 缺少 DEFAULT"}},
		},
		{
			name: "ALTER COLUMN TYPE 报错",
			sql:  "ALTER TABLE ledger.accounts ALTER COLUMN id TYPE bigint;\n",
			want: []wantV{{File: "db/migrations/0002_bad.sql", Line: 1, Msg: "ALTER COLUMN TYPE"}},
		},
		{
			name: "CREATE INDEX CONCURRENTLY 在事务迁移内必报错且不可豁免",
			sql: `-- appkit:allow-unsafe-ddl: try bypass
CREATE INDEX CONCURRENTLY idx_acc ON ledger.accounts (id);
`,
			want: []wantV{{File: "db/migrations/0002_bad.sql", Line: 2, Msg: "CREATE INDEX CONCURRENTLY"}},
		},
		{
			name: "DROP TABLE 与 TRUNCATE 未标注豁免报错",
			sql: `DROP TABLE ledger.old_table;
TRUNCATE ledger.tmp_table;
`,
			want: []wantV{
				{File: "db/migrations/0002_bad.sql", Line: 1, Msg: "DROP TABLE"},
				{File: "db/migrations/0002_bad.sql", Line: 2, Msg: "TRUNCATE"},
			},
		},
		{
			name: "表结构使用浮点列类型报错",
			sql: `CREATE TABLE ledger.metrics (
	id text PRIMARY KEY,
	amount double precision NOT NULL
);
`,
			want: []wantV{{File: "db/migrations/0002_bad.sql", Line: 1, Msg: "浮点列类型"}},
		},
		{
			name: "显式注释豁免放行",
			sql: `-- appkit:allow-unsafe-ddl: 存量数据已离线迁移完毕
DROP TABLE ledger.old_table;
-- appkit:allow-unsafe-ddl: varchar(32) 改 text 在 PG 仅改元数据
ALTER TABLE ledger.accounts ALTER COLUMN code TYPE text;
-- appkit:allow-unsafe-ddl: 地理经纬度非金额列
CREATE TABLE ledger.geo (lat double precision);
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRepo(t, map[string]string{
				"db/migrations/0002_bad.sql": tt.sql,
			})
			got, err := archcheck.CheckMigrationDDL(dir)
			if err != nil {
				t.Fatalf("CheckMigrationDDL: %v", err)
			}
			assertViolations(t, got, tt.want)
		})
	}
}
