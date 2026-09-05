package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/forgeplex/appkit/internal/agentplan"
	"github.com/forgeplex/appkit/internal/scaffold"
	"github.com/forgeplex/appkit/internal/workspace"
)

const agentResultVersion = "appkit.dev/agent-result/v1alpha1"

type agentResult struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Command    string        `json:"command"`
	OK         bool          `json:"ok"`
	Data       any           `json:"data,omitempty"`
	Error      *agentFailure `json:"error,omitempty"`
}

type agentFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type agentApplyData struct {
	PlanDigest  string                     `json:"planDigest"`
	Disposition workspace.ApplyDisposition `json:"disposition,omitempty"`
}

// A cleanup/unlock error can follow a successful commit. Preserve the engine's
// known outcome; an absent disposition must never be interpreted as no writes.
type agentApplyError struct {
	result workspace.ApplyResult
	err    error
}

func (e *agentApplyError) Error() string { return e.err.Error() }
func (e *agentApplyError) Unwrap() error { return e.err }

// agentExit is already reported as JSON; Main must not add a second stderr
// diagnostic. Existing human-oriented commands retain their output/exit codes.
type agentExit struct{ code int }

func (e *agentExit) Error() string { return "agent command failed" }

func init() {
	register(Command{Name: "plan", Summary: "生成 JSON 文件计划：sync|contract|events|errors|wrap|new|schema（schema 需授权临时库）", Run: runPlan})
	register(Command{Name: "apply", Summary: "验证并应用已审查的 JSON 计划（冲突拒绝、可恢复重放）", Run: runApply})
}

func runPlan(args []string) error {
	return runAgent("plan", args, os.Stdout, os.Stderr)
}

func runApply(args []string) error {
	return runAgent("apply", args, os.Stdout, os.Stderr)
}

func runAgent(command string, args []string, out, diagnostics io.Writer) error {
	err := executeAgent(command, args, out, diagnostics)
	if err == nil {
		return nil
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return reportAgentError(command, err, out, diagnostics)
}

func reportAgentError(command string, err error, out, diagnostics io.Writer) error {
	code, status := classifyAgentError(err)
	var data any
	var applyErr *agentApplyError
	if errors.As(err, &applyErr) {
		data = agentApplyData{applyErr.result.PlanDigest, applyErr.result.Disposition}
	}
	if reportErr := json.NewEncoder(out).Encode(agentResult{
		APIVersion: agentResultVersion, Kind: "CommandResult", Command: command,
		Data:  data,
		Error: &agentFailure{Code: code, Message: err.Error()},
	}); reportErr != nil {
		fmt.Fprintf(diagnostics, "appkit %s: write JSON result: %v\n", command, reportErr)
	}
	return &agentExit{code: status}
}

var errAgentUsage = errors.New("invalid arguments")

func executeAgent(command string, args []string, out, diagnostics io.Writer) error {
	operation := ""
	kind, name := "", ""
	if command == "plan" {
		if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
			fmt.Fprintln(diagnostics, "用法: appkit plan <sync|contract|events|errors|wrap> -dir <workspace> [flags]; appkit plan new <domain|system> <name> -dir <workspace> -target <relative-dir>; appkit plan schema -dir <workspace> -allow-temp-db [-dsn <url>]（执行可信迁移 SQL，不写仓库文件）")
			return flag.ErrHelp
		}
		if len(args) == 0 {
			return fmt.Errorf("%w: appkit plan <sync|contract|events|errors|wrap|new|schema> -dir <workspace>", errAgentUsage)
		}
		operation, args = args[0], args[1:]
		switch operation {
		case "sync", "contract", "events", "errors", "wrap", "schema":
		case "new":
			if len(args) < 2 || (args[0] != "domain" && args[0] != "system") {
				return fmt.Errorf("%w: plan new <domain|system> <name> requires -target", errAgentUsage)
			}
			kind, name, args = args[0], args[1], args[2:]
		default:
			return fmt.Errorf("%w: unsupported plan operation %q", errAgentUsage, operation)
		}
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	dir := flags.String("dir", ".", "工作区根目录")
	timeout := flags.Duration("timeout", 30*time.Second, "等待工作区锁及执行的超时")
	var input, target, planPath, expectedDigest, iface, system, module, dsn string
	var tenant, partitioned, allowTempDB bool
	if command == "plan" && operation == "schema" {
		flags.BoolVar(&allowTempDB, "allow-temp-db", false, "授权在一次性临时库执行可信迁移 SQL（不是 SQL 沙箱；需建库权限）")
		flags.StringVar(&dsn, "dsn", "", "临时库管理连接，默认 TEST_DATABASE_URL；不会写入计划")
	}
	if command == "plan" && operation != "sync" && operation != "schema" {
		flags.StringVar(&target, "target", "", "工作区相对输出路径（contract/new 是目录，其他是 .go 文件）")
		if operation != "new" {
			flags.StringVar(&input, "in", "", "工作区相对输入文件路径（wrap 显式选一个接口源文件）")
		}
		if operation == "wrap" {
			flags.StringVar(&iface, "iface", "", "契约接口名")
			flags.StringVar(&system, "system", "", "系统名")
		}
		if operation == "new" {
			flags.StringVar(&module, "module", "", "生成仓库 module path")
			flags.BoolVar(&tenant, "tenant", false, "行级租户隔离（domain）")
			flags.BoolVar(&partitioned, "partitioned", false, "schema 分区隔离（domain）")
		}
	}
	if command == "apply" {
		flags.StringVar(&planPath, "plan", "", "已审查的计划文件路径（不是目录）")
		flags.StringVar(&expectedDigest, "digest", "", "可选：必须匹配的已审查 planDigest")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %v", errAgentUsage, err)
	}
	if flags.NArg() != 0 || *timeout <= 0 {
		return fmt.Errorf("%w: unexpected positional arguments or non-positive timeout", errAgentUsage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	if command == "plan" {
		var plan workspace.Plan
		var err error
		if operation != "sync" && operation != "schema" && (target == "" || (operation != "new" && input == "")) {
			return fmt.Errorf("%w: plan %s requires -target and (except new) -in", errAgentUsage, operation)
		}
		switch operation {
		case "sync":
			plan, err = agentplan.Sync(ctx, *dir, Version())
		case "schema":
			if !allowTempDB {
				return fmt.Errorf("%w: plan schema requires -allow-temp-db to execute trusted migrations in a disposable database (not a SQL sandbox)", errAgentUsage)
			}
			if dsn == "" {
				dsn = os.Getenv("TEST_DATABASE_URL")
			}
			if dsn == "" {
				return fmt.Errorf("%w: plan schema requires -dsn or TEST_DATABASE_URL with permission to create a disposable database", errAgentUsage)
			}
			plan, err = agentplan.Schema(ctx, *dir, dsn)
		case "contract":
			plan, err = agentplan.Contract(ctx, *dir, input, target)
		case "events":
			plan, err = agentplan.Events(ctx, *dir, input, target)
		case "errors":
			plan, err = agentplan.Errors(ctx, *dir, input, target)
		case "wrap":
			if iface == "" || system == "" {
				return fmt.Errorf("%w: wrap requires -iface and -system", errAgentUsage)
			}
			plan, err = agentplan.Wrap(ctx, *dir, input, target, iface, system)
		case "new":
			plan, err = agentplan.New(ctx, *dir, target, kind, scaffold.Options{Name: name, Module: module, AppkitVersion: Version(), Tenant: tenant, Partitioned: partitioned})
		}
		if err != nil {
			return err
		}
		encoded, err := workspace.MarshalPlan(plan)
		if err != nil {
			return err
		}
		n, err := out.Write(encoded)
		if err == nil && n != len(encoded) {
			return io.ErrShortWrite
		}
		return err
	}
	if planPath == "" {
		return fmt.Errorf("%w: apply requires -plan", errAgentUsage)
	}
	encoded, err := readPlanFile(planPath)
	if err != nil {
		return err
	}
	plan, err := workspace.ParsePlan(encoded)
	if err != nil {
		return err
	}
	if expectedDigest != "" && plan.Digest() != expectedDigest {
		return fmt.Errorf("%w: plan does not match -digest", workspace.ErrInvalidPlanDocument)
	}
	result, err := workspace.Apply(ctx, *dir, plan)
	if err != nil {
		return &agentApplyError{result: result, err: err}
	}
	return json.NewEncoder(out).Encode(agentResult{
		APIVersion: agentResultVersion, Kind: "CommandResult", Command: command, OK: true,
		Data: agentApplyData{result.PlanDigest, result.Disposition},
	})
}

func readPlanFile(name string) ([]byte, error) {
	before, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > workspace.MaxPlanDocumentBytes {
		return nil, fmt.Errorf("%w: expected bounded regular plan file (no symlinks)", workspace.ErrInvalidPlanDocument)
	}
	file, err := openPlanFile(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > workspace.MaxPlanDocumentBytes || !os.SameFile(before, info) {
		return nil, fmt.Errorf("%w: expected bounded regular plan file", workspace.ErrInvalidPlanDocument)
	}
	content, err := io.ReadAll(io.LimitReader(file, workspace.MaxPlanDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > workspace.MaxPlanDocumentBytes {
		return nil, workspace.ErrInvalidPlanDocument
	}
	return content, nil
}

func classifyAgentError(err error) (string, int) {
	switch {
	case errors.Is(err, errAgentUsage):
		return "invalid_arguments", 2
	case errors.Is(err, workspace.ErrRecovery), errors.Is(err, workspace.ErrRollback), errors.Is(err, workspace.ErrRecoveryRestart):
		return "recovery_required", 4
	case errors.Is(err, workspace.ErrChanged):
		return "workspace_conflict", 3
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled", 5
	case errors.Is(err, workspace.ErrInvalidPlanDocument), errors.Is(err, workspace.ErrNonCanonicalPlan), errors.Is(err, workspace.ErrInvalidPlan), errors.Is(err, workspace.ErrInvalidChange), errors.Is(err, workspace.ErrInvalidPath), errors.Is(err, workspace.ErrSymlink):
		return "invalid_plan", 2
	case errors.Is(err, workspace.ErrLockUnsupported):
		return "locking_unsupported", 1
	default:
		return "operation_failed", 1
	}
}
