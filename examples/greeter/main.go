// Command greeter 是 appkit 的最小完整示例（不依赖数据库）：
// 两个业务模块（greet、gateway）装配进同一个二进制，用 -target 在
// 「单体」与「拆分」两种部署形态间切换——部署形态是启动参数，
// 不是架构决策（DESIGN §5）。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/config"
	"github.com/forgeplex/appkit/examples/greeter/gateway"
	"github.com/forgeplex/appkit/examples/greeter/greet"
	"github.com/forgeplex/appkit/examples/greeter/greetapi"
	"github.com/forgeplex/appkit/httpserver"
	"github.com/forgeplex/appkit/telemetry"
)

// greeterConfig 演示分层加载：greeter.yaml（可缺省）→ GREETER_* 环境变量覆盖。
type greeterConfig struct {
	Addr string    `koanf:"addr"`
	Log  logConfig `koanf:"log"`
}

type logConfig struct {
	Level  string `koanf:"level" validate:"omitempty,oneof=debug info warn error"`
	Format string `koanf:"format" validate:"omitempty,oneof=json text"`
}

func main() {
	target := flag.String("target", "all",
		"本地实例化的模块集：all 或逗号分隔的模块名（greet,gateway）")
	flag.Parse()

	if err := run(*target); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(target string) error {
	ctx := context.Background()

	cfg, err := config.Load[greeterConfig](config.Options{
		Files:     []string{"greeter.yaml"},
		EnvPrefix: "GREETER",
		Optional:  true,
	})
	if err != nil {
		return fmt.Errorf("greeter: 加载配置: %w", err)
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}

	tel, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: "greeter",
		Env:         "dev",
		LogLevel:    cfg.Log.Level,
		LogFormat:   cfg.Log.Format,
	})
	if err != nil {
		return fmt.Errorf("greeter: 初始化遥测: %w", err)
	}
	log := tel.Logger

	app := appkit.New(
		[]appkit.Module{
			greet.Module(log),
			gateway.Module(log),
		},
		appkit.Target(target),
		appkit.HTTPAddr(cfg.Addr),
		appkit.Logger(log),
		appkit.Middleware(httpserver.Base(log)...),
		// greet 不在 target 集内时，gateway 对 greetapi.Service 的 Resolve
		// 落到这个远程兜底。真实项目传合约仓库生成的 HTTP client 构造器
		//（实现同一接口），示例用假 client 代替以免起第二个进程。
		appkit.Remote(func(*appkit.Registry) (greetapi.Service, error) {
			return remoteClient{}, nil
		}),
	)
	runErr := app.Run(ctx)

	// 遥测最后关：关停钩子产生的 span/metric 也要被 flush。
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tel.Shutdown(sctx); err != nil {
		log.Error("greeter: 遥测关停失败", "err", err)
	}
	if runErr != nil {
		return fmt.Errorf("greeter: %w", runErr)
	}
	return nil
}

// remoteClient 顶替合约仓库生成的 HTTP client 的位置：实现同一个
// greetapi.Service。真实生成物会请求 greet 服务并用 apperr.FromProblem 把
// problem+json 重建回 *apperr.Error——错误码跨网络不变，gateway 无感切换。
// 示例就地返回可辨识的应答，以免要求起第二个进程。
type remoteClient struct{}

func (remoteClient) Greet(_ context.Context, req greetapi.GreetRequest) (greetapi.GreetReply, error) {
	if req.Name == "" {
		return greetapi.GreetReply{}, apperr.InvalidArgument("name 不能为空")
	}
	return greetapi.GreetReply{Message: fmt.Sprintf("Hello, %s! (via remote greet)", req.Name)}, nil
}
