package archcheck_test

import (
	"testing"

	"github.com/forgeplex/appkit/internal/archcheck"
)

func TestCheckQueries(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want []wantV
	}{
		{
			name: "显式列、COUNT(*) 与带 WHERE 的写查询零违规",
			sql: `-- name: CountUsers :one
SELECT count(*) FROM ledger.users;

-- name: GetUser :one
SELECT id, email FROM ledger.users WHERE id = $1;

-- name: DeleteUser :exec
DELETE FROM ledger.users WHERE id = $1;

-- name: UpdateUser :exec
UPDATE ledger.users SET status = $2 WHERE id = $1;
`,
		},
		{
			name: "SELECT * 与 SELECT alias.* 报错",
			sql: `-- name: AllUsers :many
SELECT * FROM ledger.users;
-- name: AllAlias :many
SELECT u.* FROM ledger.users u;
`,
			want: []wantV{
				{File: "db/queries/q.sql", Line: 2, Msg: "禁止 SELECT *"},
				{File: "db/queries/q.sql", Line: 4, Msg: "禁止 SELECT *"},
			},
		},
		{
			name: "缺少 WHERE 的 UPDATE / DELETE 报错",
			sql: `UPDATE ledger.users SET status = 'archived';
DELETE FROM ledger.users;
`,
			want: []wantV{
				{File: "db/queries/q.sql", Line: 1, Msg: "缺少 WHERE"},
				{File: "db/queries/q.sql", Line: 2, Msg: "缺少 WHERE"},
			},
		},
		{
			name: "显式注释豁免放行",
			sql: `-- appkit:allow-unsafe-sql: 全表清理测试表
DELETE FROM ledger.temp_work;
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRepo(t, map[string]string{"db/queries/q.sql": tt.sql})
			got, err := archcheck.CheckQueries(dir)
			if err != nil {
				t.Fatalf("CheckQueries: %v", err)
			}
			assertViolations(t, got, tt.want)
		})
	}
}
