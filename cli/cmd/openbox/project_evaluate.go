package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/evaluate"
)

const projectEvaluateUsage = "Usage: openbox project evaluate --image IMAGE --env-file FILE --openbox-agent AGENT_ID --output DIR\n"

type projectEvaluateOptions struct {
	image        string
	envFile      string
	openboxAgent string
	output       string
}

type onceStringFlag struct {
	name  string
	value *string
	seen  bool
}

func (value *onceStringFlag) String() string {
	if value.value == nil {
		return ""
	}
	return *value.value
}

func (value *onceStringFlag) Set(input string) error {
	if value.seen {
		return fmt.Errorf("flag --%s may be specified only once", value.name)
	}
	value.seen = true
	*value.value = input
	return nil
}

func (a *app) parseProjectEvaluateArgs(args []string) (projectEvaluateOptions, int, bool) {
	fs := a.newFlagSet("openbox project evaluate")
	options := projectEvaluateOptions{}
	flags := []*onceStringFlag{
		{name: "image", value: &options.image},
		{name: "env-file", value: &options.envFile},
		{name: "openbox-agent", value: &options.openboxAgent},
		{name: "output", value: &options.output},
	}
	for _, value := range flags {
		fs.Var(value, value.name, projectEvaluateFlagHelp(value.name))
	}
	fs.Usage = func() {
		fmt.Fprint(a.stderr, projectEvaluateUsage)
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, args); !ok {
		return projectEvaluateOptions{}, code, false
	}
	if fs.NArg() != 0 {
		fmt.Fprint(a.stderr, projectEvaluateUsage)
		return projectEvaluateOptions{}, a.errorf("project evaluate rejects positional arguments"), false
	}
	for _, value := range flags {
		if !value.seen || *value.value == "" {
			fmt.Fprint(a.stderr, projectEvaluateUsage)
			return projectEvaluateOptions{}, a.errorf("project evaluate requires --%s", value.name), false
		}
	}
	return options, exitOK, true
}

func projectEvaluateFlagHelp(name string) string {
	switch name {
	case "image":
		return "local OCI image reference"
	case "env-file":
		return "strict non-secret evaluation environment file"
	case "openbox-agent":
		return "pre-existing dedicated evaluation agent UUID"
	case "output":
		return "new diagnostic or sealed-observation output directory"
	default:
		return ""
	}
}

func (a *app) runProjectEvaluate(args []string) int {
	options, code, ok := a.parseProjectEvaluateArgs(args)
	if !ok {
		return code
	}
	runner := a.runProjectEvaluation
	if runner == nil {
		runner = func(ctx context.Context, input evaluate.Input) (evaluate.Result, error) {
			return evaluate.Run(ctx, input, evaluate.SystemDependencies())
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	result, err := runner(ctx, evaluate.Input{
		Image: options.image, EnvFile: options.envFile,
		OpenBoxAgent: options.openboxAgent, Output: options.output,
		BackendURL:          inputEnvironment(a.getenv, devconfig.EnvBackendURL),
		ControlToken:        inputEnvironment(a.getenv, devconfig.EnvControlToken),
		ObservationRequired: true,
		ProxyConfigured:     proxyEnvironmentConfigured(a.getenv),
	})
	if err != nil {
		if result.Output != "" {
			fmt.Fprintf(a.stderr, "project evaluation output retained: %s\n", result.Output)
		}
		return a.errorf("%v", err)
	}
	fmt.Fprintf(a.stdout, "project observation sealed: %s\n", result.Output)
	return exitOK
}

func inputEnvironment(getenv func(string) string, name string) string {
	if getenv == nil {
		return ""
	}
	return getenv(name)
}

func proxyEnvironmentConfigured(getenv func(string) string) bool {
	if getenv == nil {
		return false
	}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if getenv(name) != "" {
			return true
		}
	}
	return false
}
