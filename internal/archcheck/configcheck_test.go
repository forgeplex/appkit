package archcheck_test

import (
	"testing"

	"github.com/forgeplex/appkit/internal/archcheck"
)

func TestCheckConfigs(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []wantV
	}{
		{
			name: "dev 允许 disabled 与本地 http，prod 正确配置零违规",
			files: map[string]string{
				"config/dev.yaml": "env: dev\nsecurity:\n  mode: disabled\ndebug:\n  pprof: true\n",
				"config/prod.yaml": `env: prod
security:
  mode: user_facing
  oidc:
    realms:
      - issuer: https://sso.example.com/
        jwks_url: https://sso.example.com/.well-known/jwks.json
debug:
  pprof: false
`,
			},
		},
		{
			name: "prod 禁 disabled、禁 user_facing+pprof、禁明文 http OIDC",
			files: map[string]string{
				"config/staging.yaml": `env: staging
security:
  mode: disabled
`,
				"config/prod.yaml": `env: prod
security:
  mode: user_facing
  oidc:
    realms:
      - issuer: http://sso.example.com/
        jwks_url: http://sso.example.com/jwks.json
debug:
  pprof: true
`,
			},
			want: []wantV{
				{File: "config/prod.yaml", Msg: "禁止在 security.mode=user_facing 下开启 debug.pprof"},
				{File: "config/prod.yaml", Msg: "OIDC issuer"},
				{File: "config/prod.yaml", Msg: "OIDC jwks_url"},
				{File: "config/staging.yaml", Msg: "禁止 security.mode: disabled"},
			},
		},
		{
			name: "任意配置禁提交明文私钥",
			files: map[string]string{
				"config/dev.yaml": "env: dev\nkey: |\n  -----BEGIN PRIVATE KEY-----\n  abc\n  -----END PRIVATE KEY-----\n",
			},
			want: []wantV{
				{File: "config/dev.yaml", Msg: "明文 PEM 私钥"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRepo(t, tt.files)
			got, err := archcheck.CheckConfigs(dir)
			if err != nil {
				t.Fatalf("CheckConfigs: %v", err)
			}
			assertViolations(t, got, tt.want)
		})
	}
}
