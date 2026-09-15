package main

import (
	"fmt"
	"path/filepath"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/observation"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/runfs"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/securityreport"
)

const projectVerifyUsage = "Usage: openbox project verify PACK\n"

func (a *app) runProjectVerify(args []string) int {
	fs := a.newFlagSet("openbox project verify")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprint(a.stderr, projectVerifyUsage)
		return exitError
	}
	root, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return a.errorf("project verify: resolve pack path: %v", err)
	}
	discriminator, err := runfs.ReadCommittedManifestDiscriminator(root)
	if err != nil {
		return a.errorf("project verify: %v", err)
	}
	switch discriminator.PackSchema() {
	case runfs.AuditPackSchema:
		verified, verifyErr := runfs.VerifyPack(root)
		if verifyErr != nil {
			return a.errorf("project verify: %v", verifyErr)
		}
		if err := runfs.RecheckCommittedManifest(root, discriminator); err != nil {
			return a.errorf("project verify: %v", err)
		}
		fmt.Fprintf(a.stdout, "audit pack objects verified: %s\n", verified.Digest())
		fmt.Fprintf(a.stdout, "  roles: %d\n", verified.RoleCount())
		fmt.Fprintln(a.stdout, "note: point-in-time canonical manifest structure, role encodings, lengths, CIDs, and exact object set were verified; public-schema conformance remains a separate contract check")
	case runfs.ObservationPackSchema:
		pack, verifyErr := observation.Read(root)
		if verifyErr != nil {
			return a.errorf("project verify: %v", verifyErr)
		}
		digest, digestErr := pack.PackDigest()
		if digestErr != nil {
			return a.errorf("project verify: %v", digestErr)
		}
		if err := runfs.RecheckCommittedManifest(root, discriminator); err != nil {
			return a.errorf("project verify: %v", err)
		}
		fmt.Fprintln(a.stdout, "project observation verified: ai.openbox.project-observation/v1")
		fmt.Fprintf(a.stdout, "  pack_digest: %s\n", digest)
	case runfs.SecurityReportPackSchema:
		pack, verifyErr := securityreport.Verify(root)
		if verifyErr != nil {
			return a.errorf("project verify: %v", verifyErr)
		}
		if err := runfs.RecheckCommittedManifest(root, discriminator); err != nil {
			return a.errorf("project verify: %v", err)
		}
		fmt.Fprintln(a.stdout, "project security report verified: ai.openbox.project-security-report/v1")
		fmt.Fprintf(a.stdout, "  pack_digest: %s\n", pack.PackDigest)
	default:
		return a.errorf("project verify: unknown pack schema %q", discriminator.PackSchema())
	}
	return exitOK
}
