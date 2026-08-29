package archcheck_test

import (
	"testing"

	"github.com/forgeplex/appkit/internal/archcheck"
)

func TestCheckMigrations(t *testing.T) {
	tests := []struct {
		name  string
		files []string // db/migrations 下的文件名
		want  []wantV
	}{
		{
			name:  "序号递增可有空洞",
			files: []string{"0001_init.sql", "0002_outbox.sql", "0010_hold.sql"},
			want:  nil,
		},
		{
			name:  "目录不存在无违规",
			files: nil,
			want:  nil,
		},
		{
			name:  "序号位数不足违规",
			files: []string{"001_init.sql"},
			want:  []wantV{{File: "db/migrations/001_init.sql", Msg: "NNNN_描述.sql"}},
		},
		{
			name:  "缺下划线描述违规",
			files: []string{"0001.sql", "0002-init.sql"},
			want: []wantV{
				{File: "db/migrations/0001.sql", Msg: "NNNN_描述.sql"},
				{File: "db/migrations/0002-init.sql", Msg: "NNNN_描述.sql"},
			},
		},
		{
			name:  "无序号前缀违规",
			files: []string{"init.sql"},
			want:  []wantV{{File: "db/migrations/init.sql", Msg: "NNNN_描述.sql"}},
		},
		{
			name:  "序号重复违规",
			files: []string{"0001_init.sql", "0002_a.sql", "0002_b.sql", "0003_c.sql"},
			want:  []wantV{{File: "db/migrations/0002_b.sql", Msg: "迁移序号 0002 与 0002_a.sql 重复"}},
		},
		{
			name:  "连续重复逐条报",
			files: []string{"0002_a.sql", "0002_b.sql", "0002_c.sql"},
			want: []wantV{
				{File: "db/migrations/0002_b.sql", Msg: "重复"},
				{File: "db/migrations/0002_c.sql", Msg: "重复"},
			},
		},
		{
			name:  "非 sql 文件忽略",
			files: []string{"0001_init.sql", "README.md"},
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := map[string]string{}
			for _, f := range tt.files {
				m["db/migrations/"+f] = "-- fixture\n"
			}
			dir := writeRepo(t, m)
			got, err := archcheck.CheckMigrations(dir)
			if err != nil {
				t.Fatalf("CheckMigrations: %v", err)
			}
			assertViolations(t, got, tt.want)
		})
	}
}
