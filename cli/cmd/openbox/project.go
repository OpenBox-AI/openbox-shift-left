package main

import "fmt"

const projectUsage = `Usage:
  openbox project inspect [path] [--output DIR]
  openbox project evaluate --image IMAGE --env-file FILE --openbox-agent AGENT_ID --output DIR
	openbox project finalize --evaluation OBSERVATION_PACK --analysis CANDIDATE_JSON --output REPORT_PACK
  openbox project verify PACK
  openbox project report --pack DIR [--format markdown|json|sarif]
  openbox project propose --pack DIR [--format json|markdown]

Current limits:
  Project evaluation runs one self-starting local image in pinned OpenShell.
  Its staging execution record is not an audit pack or security report.
  The Mastra image is a development conformance asset, not a customer report source.
  Historical verified packs remain readable with project verify/report/propose.
`

func (a *app) runProject(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, projectUsage)
		return exitError
	}
	switch args[0] {
	case "inspect":
		return a.runProjectInspect(args[1:])
	case "evaluate":
		return a.runProjectEvaluate(args[1:])
	case "finalize":
		return a.runProjectFinalize(args[1:])
	case "verify":
		return a.runProjectVerify(args[1:])
	case "report":
		return a.runProjectReport(args[1:])
	case "propose":
		return a.runProjectPropose(args[1:])
	case "help", "--help", "-h":
		fmt.Fprint(a.stderr, projectUsage)
		return exitOK
	default:
		fmt.Fprint(a.stderr, projectUsage)
		return a.errorf("unknown project subcommand %q", args[0])
	}
}
