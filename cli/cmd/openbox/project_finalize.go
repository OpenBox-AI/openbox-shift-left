package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/securityreport"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/targetposture"
)

const projectFinalizeUsage = "Usage: openbox project finalize --evaluation OBSERVATION_PACK --analysis CANDIDATE_JSON --output REPORT_PACK\n"

type projectFinalizeOptions struct {
	evaluation string
	analysis   string
	output     string
}

func (a *app) parseProjectFinalizeArgs(args []string) (projectFinalizeOptions, int, bool) {
	fs := a.newFlagSet("openbox project finalize")
	options := projectFinalizeOptions{}
	flags := []*onceStringFlag{
		{name: "evaluation", value: &options.evaluation},
		{name: "analysis", value: &options.analysis},
		{name: "output", value: &options.output},
	}
	for _, value := range flags {
		fs.Var(value, value.name, "required Phase 4 input/output path")
	}
	fs.Usage = func() {
		fmt.Fprint(a.stderr, projectFinalizeUsage)
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, args); !ok {
		return projectFinalizeOptions{}, code, false
	}
	if fs.NArg() != 0 {
		fmt.Fprint(a.stderr, projectFinalizeUsage)
		return projectFinalizeOptions{}, a.errorf("project finalize rejects positional arguments"), false
	}
	for _, value := range flags {
		if !value.seen || *value.value == "" {
			fmt.Fprint(a.stderr, projectFinalizeUsage)
			return projectFinalizeOptions{}, a.errorf("project finalize requires --%s", value.name), false
		}
	}
	return options, exitOK, true
}

func (a *app) runProjectFinalize(args []string) int {
	options, code, ok := a.parseProjectFinalizeArgs(args)
	if !ok {
		return code
	}
	// This complete offline gate intentionally precedes every credential lookup,
	// network call, and output creation.
	prepared, err := securityreport.Prepare(options.evaluation, options.analysis, options.output)
	if err != nil {
		return a.errorf("project finalize: %v", err)
	}
	runner := a.runProjectFinalization
	if runner == nil {
		runner = func(ctx context.Context, prepared *securityreport.Prepared, input securityreport.RuntimeInput) (securityreport.Result, error) {
			return securityreport.Finalize(ctx, prepared, input, securityreport.Dependencies{})
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	result, err := runner(ctx, prepared, securityreport.RuntimeInput{
		BackendURL:      inputEnvironment(a.getenv, devconfig.EnvBackendURL),
		ControlToken:    inputEnvironment(a.getenv, devconfig.EnvControlToken),
		ProxyConfigured: proxyEnvironmentConfigured(a.getenv),
		HTTP:            targetposture.NewHTTPClient(),
	})
	if err != nil {
		return a.errorf("project finalize: %v", err)
	}
	fmt.Fprintf(a.stdout, "project security report sealed: %s\n", result.Output)
	fmt.Fprintf(a.stdout, "  pack_digest: %s\n", result.PackDigest)
	return exitOK
}
