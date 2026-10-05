package main

import "fmt"

const projectUsage = `Usage:
  openbox project evaluate --image IMAGE --env-file FILE --openbox-agent AGENT_ID [--wait]

Project evaluation runs one self-starting local image in the OpenBox Sandbox,
then asks the OpenBox backend to evaluate the run. It needs OPENBOX_CONTROL_TOKEN
with permission evaluate:agent_security and a model connector configured by the
organization. It writes no files; --wait prints the result.
`

func (a *app) runProject(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, projectUsage)
		return exitError
	}
	switch args[0] {
	case "evaluate":
		return a.runProjectEvaluate(args[1:])
	case "help", "--help", "-h":
		fmt.Fprint(a.stderr, projectUsage)
		return exitOK
	default:
		fmt.Fprint(a.stderr, projectUsage)
		return a.errorf("unknown project subcommand %q", args[0])
	}
}
