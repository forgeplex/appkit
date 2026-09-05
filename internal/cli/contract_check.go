package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/forgeplex/appkit/internal/gen"
)

func init() {
	register(Command{Name: "contract-check", Summary: "只读比较两个 contract.yaml 的保守兼容规则（JSON）", Run: runContractCheck})
}

func runContractCheck(args []string) error { return contractCheck(args, os.Stdout, os.Stderr) }

func contractCheck(args []string, out, diagnostics io.Writer) error {
	flags := flag.NewFlagSet("contract-check", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	base := flags.String("base", "", "旧版 contract.yaml")
	candidate := flags.String("candidate", "", "候选版 contract.yaml")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return reportAgentError("contract-check", fmt.Errorf("%w: %v", errAgentUsage, err), out, diagnostics)
	}
	if *base == "" || *candidate == "" || flags.NArg() != 0 {
		return reportAgentError("contract-check", fmt.Errorf("%w: requires -base and -candidate", errAgentUsage), out, diagnostics)
	}
	// Reuse the bounded regular-file reader; YAML is parsed from these exact bytes.
	old, err := readPlanFile(*base)
	if err == nil {
		var next []byte
		next, err = readPlanFile(*candidate)
		if err == nil {
			err = gen.CheckContractCompatibilitySources(*base, old, *candidate, next)
		}
	}
	if err != nil {
		var compatibility *gen.ContractCompatibilityError
		if !errors.As(err, &compatibility) {
			return reportAgentError("contract-check", err, out, diagnostics)
		}
		if err := json.NewEncoder(out).Encode(agentResult{
			APIVersion: agentResultVersion, Kind: "CommandResult", Command: "contract-check",
			Data:  compatibility,
			Error: &agentFailure{Code: "contract_incompatible", Message: err.Error()},
		}); err != nil {
			return err
		}
		return &agentExit{code: 3}
	}
	return json.NewEncoder(out).Encode(agentResult{APIVersion: agentResultVersion, Kind: "CommandResult", Command: "contract-check", OK: true})
}
