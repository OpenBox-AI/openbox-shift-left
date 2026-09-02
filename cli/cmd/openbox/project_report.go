package main

import (
	"errors"
	"fmt"
	"path/filepath"

	assurancereport "github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/report"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/runfs"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/securityreport"
)

const (
	projectReportUsage  = "Usage: openbox project report --pack DIR [--format markdown|json|sarif]\n"
	projectProposeUsage = "Usage: openbox project propose --pack DIR [--format json|markdown]\n"
)

type projectPackOutputOptions struct {
	pack   string
	format string
}

func (a *app) runProjectReport(args []string) int {
	options, code, ok := a.parseProjectPackOutput(args, "report", "markdown", map[string]bool{"markdown": true, "json": true, "sarif": true})
	if !ok {
		return code
	}
	root, err := filepath.Abs(options.pack)
	if err != nil {
		return a.errorf("project report: %v", err)
	}
	discriminator, err := runfs.ReadCommittedManifestDiscriminator(root)
	if err != nil {
		return a.errorf("project report: %v", err)
	}
	var content []byte
	switch discriminator.PackSchema() {
	case runfs.AuditPackSchema:
		pack, err := validateProjectPack(root)
		if err != nil {
			return a.errorf("project report: %v", err)
		}
		projections, err := assurancereport.Render(pack)
		if err != nil {
			return a.errorf("project report: %v", err)
		}
		switch options.format {
		case "json":
			content = projections.JSON()
		case "sarif":
			content = projections.SARIF()
		default:
			content = projections.Markdown()
		}
	case runfs.SecurityReportPackSchema:
		pack, err := securityreport.Verify(root)
		if err != nil {
			return a.errorf("project report: %v", err)
		}
		switch options.format {
		case "json":
			content = pack.Projection.JSON
		case "sarif":
			content = pack.Projection.SARIF
		default:
			content = pack.Projection.Markdown
		}
	default:
		return a.errorf("project report: pack schema %q does not support reports", discriminator.PackSchema())
	}
	if err := runfs.RecheckCommittedManifest(root, discriminator); err != nil {
		return a.errorf("project report: %v", err)
	}
	return a.writeProjectPackOutput("report", content)
}

func (a *app) runProjectPropose(args []string) int {
	options, code, ok := a.parseProjectPackOutput(args, "propose", "json", map[string]bool{"json": true, "markdown": true})
	if !ok {
		return code
	}
	pack, err := validateProjectPack(options.pack)
	if err != nil {
		return a.errorf("project propose: %v", err)
	}
	proposal, err := assurancereport.CompileProposal(pack)
	if err != nil {
		return a.errorf("project propose: %v", err)
	}
	var content []byte
	if options.format == "markdown" {
		content, err = proposal.Markdown()
	} else {
		content, err = proposal.JSON()
	}
	if err != nil {
		return a.errorf("project propose: %v", err)
	}
	return a.writeProjectPackOutput("propose", content)
}

func (a *app) parseProjectPackOutput(args []string, command, defaultFormat string, formats map[string]bool) (projectPackOutputOptions, int, bool) {
	options := projectPackOutputOptions{format: defaultFormat}
	fs := a.newFlagSet("openbox project " + command)
	fs.StringVar(&options.pack, "pack", "", "read the authoritative local audit pack from DIR")
	fs.StringVar(&options.format, "format", defaultFormat, "select the stdout projection")
	fs.Usage = func() {
		if command == "report" {
			fmt.Fprint(a.stderr, projectReportUsage)
		} else {
			fmt.Fprint(a.stderr, projectProposeUsage)
		}
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, args); !ok {
		return projectPackOutputOptions{}, code, false
	}
	if fs.NArg() != 0 || options.pack == "" || !formats[options.format] {
		return projectPackOutputOptions{}, a.errorf("project %s: requires --pack DIR and a supported --format", command), false
	}
	return options, exitOK, true
}

func validateProjectPack(path string) (*assurancereport.ValidatedPack, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve pack path: %w", err)
	}
	verified, err := runfs.VerifyPack(root)
	if err != nil {
		return nil, err
	}
	return assurancereport.ValidatePack(verified)
}

func (a *app) writeProjectPackOutput(command string, content []byte) int {
	written, err := a.stdout.Write(content)
	if err != nil || written != len(content) {
		if err == nil {
			err = errors.New("short write")
		}
		return a.errorf("project %s: write output: %v", command, err)
	}
	return exitOK
}
