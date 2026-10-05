package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/evaluate"
)

const projectEvaluateUsage = "Usage: openbox project evaluate --image IMAGE --env-file FILE --openbox-agent AGENT_ID [--wait]\n"

type projectEvaluateOptions struct {
	image        string
	envFile      string
	openboxAgent string
	wait         bool
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
	}
	for _, value := range flags {
		fs.Var(value, value.name, projectEvaluateFlagHelp(value.name))
	}
	fs.BoolVar(&options.wait, "wait", false, "wait for the security evaluation and print a summary (exit non-zero if it fails)")
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
		return "sandbox environment file (.env.sandbox); credential-shaped names reach the workload in plaintext and are warned about"
	case "openbox-agent":
		return "pre-existing dedicated evaluation agent UUID"
	default:
		return ""
	}
}

func (a *app) runProjectEvaluate(args []string) int {
	options, code, ok := a.parseProjectEvaluateArgs(args)
	if !ok {
		return code
	}
	// One wiring point, in main.go. A second construction here would be a
	// second place for the sandbox attachment to drift out of step.
	runner := a.runProjectEvaluation
	if runner == nil {
		return a.errorf("project evaluate: evaluation runner is not configured")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	_, err := runner(ctx, evaluate.Input{
		Image: options.image, EnvFile: options.envFile,
		OpenBoxAgent: options.openboxAgent, Wait: options.wait,
		// The connector. Env wins, then dev.json, then the local-stack default
		// inside evaluate — the same precedence auth and init already use, so a
		// developer pointed at UAT by `openbox auth` stays pointed at UAT here
		// without passing a flag.
		CoreURL:         inputEnvironment(a.getenv, devconfig.EnvBaseURL),
		BackendURL:      inputEnvironment(a.getenv, devconfig.EnvBackendURL),
		OpenBoxProvider: inputEnvironment(a.getenv, "OPENBOX_SANDBOX_PROVIDER"),
		ControlToken:    inputEnvironment(a.getenv, devconfig.EnvControlToken),
		// The request line, the --wait summary and the credential warnings are
		// printed by the evaluation as it goes, so a long --wait shows progress.
		Stdout: a.stdout,
		Stderr: a.stderr,
	})
	if err != nil {
		class, message := evaluate.Describe(err)
		return a.errorf("project evaluate failed (%s): %s", class, message)
	}
	return exitOK
}

func inputEnvironment(getenv func(string) string, name string) string {
	if getenv == nil {
		return ""
	}
	return getenv(name)
}
