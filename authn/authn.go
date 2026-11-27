// Package authn 是用户与服务访问令牌的验签机制件：
//   - 用户令牌经 OIDCVerifier（多 Realm JWKS / 静态公钥，支持 RS256/ES256/EdDSA、
//     org_id 租户映射、Realm Partition 绑定与 MFA step-up）解析并注入 appkit.Actor；
//   - 服务间调用经 ServiceVerifier 解析短期服务 JWT 并注入 appkit.ServicePrincipal。
//
// 权限判定（Require/Check）在框架根包，本包只负责「凭证 → Principal/Actor」。
package authn

import (
	"errors"
	"net/http"

	"github.com/forgeplex/appkit/apperr"
)

var errEmptySubject = errors.New("令牌 sub 为空")

func writeInvalid(w http.ResponseWriter, err error) {
	apperr.WriteProblem(w, apperr.Unauthenticated("invalid or expired credential").WithCause(err))
}
