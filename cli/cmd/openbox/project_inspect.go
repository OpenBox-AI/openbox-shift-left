package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	assuranceinspect "github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/inspect"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/model"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/runfs"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/sdkdesc"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/snapshot"
)

type projectInspectOptions struct {
	path   string
	output string
}

type passiveProjectInspection struct {
	root     string
	project  model.ProjectArtifacts
	coverage sdkdesc.CoverageArtifacts
}

func buildProjectInspection(root string, copied *snapshot.Snapshot, identity model.ProjectIdentity) (passiveProjectInspection, model.Graph, error) {
	return buildProjectInspectionFrom(root, copied, copied, identity)
}

// buildProjectInspectionFrom lets a trusted execution bind its complete
// dependency-bearing snapshot while keeping passive lexical discovery on the
// authored-source selection. Dependency bytes are execution identity, not
// 28,000 additional application manifests or source files.
func buildProjectInspectionFrom(root string, discovery, bound *snapshot.Snapshot, identity model.ProjectIdentity) (passiveProjectInspection, model.Graph, error) {
	detection, err := assuranceinspect.Detect(discovery)
	if err != nil {
		return passiveProjectInspection{}, model.Graph{}, err
	}
	graph, err := model.Normalize(detection)
	if err != nil {
		return passiveProjectInspection{}, model.Graph{}, err
	}
	compatibility := sdkdesc.ValidateStaticProject(graph)
	expected, err := sdkdesc.DeriveExpectedCoverage(graph, compatibility)
	if err != nil {
		return passiveProjectInspection{}, model.Graph{}, err
	}
	projectArtifacts, err := model.BuildProjectArtifacts(bound, graph, identity)
	if err != nil {
		return passiveProjectInspection{}, model.Graph{}, err
	}
	coverageArtifacts, err := sdkdesc.BuildCoverageArtifacts(projectArtifacts, expected)
	if err != nil {
		return passiveProjectInspection{}, model.Graph{}, err
	}
	return passiveProjectInspection{root: root, project: projectArtifacts, coverage: coverageArtifacts}, graph, nil
}

func (a *app) runProjectInspect(args []string) int {
	options, code, ok := a.parseProjectInspectArgs(args)
	if !ok {
		return code
	}
	if err := ensureInspectionOutputSupported(); err != nil {
		return a.errorf("project inspect: %v", err)
	}
	outputBoundary := options.output
	var createdParents []string
	if outputBoundary != "" {
		absolute, err := filepath.Abs(outputBoundary)
		if err != nil {
			return a.errorf("project inspect: resolve output: %v", err)
		}
		outputBoundary = absolute
	} else {
		project, err := snapshot.Resolve(options.path, snapshot.Boundaries{})
		if err != nil {
			return a.errorf("project inspect: %v", err)
		}
		_, createdParents, err = ensureInspectionParent(project.Root())
		if err != nil {
			err = errors.Join(err, removeEmptyInspectionParents(createdParents))
			return a.errorf("project inspect: %v", err)
		}
	}
	inspection, err := inspectProjectPassively(options.path, outputBoundary)
	if err != nil {
		err = errors.Join(err, removeEmptyInspectionParents(createdParents))
		return a.errorf("project inspect: %v", err)
	}
	output, err := writeProjectInspection(inspection, outputBoundary)
	if err != nil {
		err = errors.Join(err, removeEmptyInspectionParents(createdParents))
		return a.errorf("project inspect: %v", err)
	}
	fmt.Fprintf(a.stdout, "project inspection written: %s\n", output)
	fmt.Fprintf(a.stdout, "  project-snapshot %s\n", inspection.project.SnapshotManifest().Digest())
	fmt.Fprintf(a.stdout, "  project-model %s\n", inspection.project.ProjectModel().Digest())
	fmt.Fprintf(a.stdout, "  sdk-coverage %s\n", inspection.coverage.SDKCoverage.Digest())
	fmt.Fprintf(a.stdout, "  readiness %s: %s\n", inspection.coverage.Guidance.Status, inspection.coverage.Guidance.Summary)
	fmt.Fprintln(a.stdout, "note: standalone inspection output is not an audit pack and contains no manifest.json")
	return exitOK
}

// inspectProjectPassively runs the complete in-memory Phase 02 pipeline. It
// performs no durable output transaction itself. The temporary executable
// snapshot is owned by a runfs workspace and removed before return.
func inspectProjectPassively(path, outputBoundary string) (result passiveProjectInspection, err error) {
	temporaryParent, err := os.MkdirTemp("", "openbox-project-inspect-")
	if err != nil {
		return result, fmt.Errorf("project inspect: create temporary parent: %w", err)
	}
	defer func() {
		if removeErr := os.Remove(temporaryParent); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("project inspect: remove empty temporary parent: %w", removeErr))
		}
	}()
	temporaryWorkspace, err := runfs.Create(filepath.Join(temporaryParent, "workspace"))
	if err != nil {
		return result, fmt.Errorf("project inspect: create temporary workspace: %w", err)
	}
	defer func() {
		if _, cleanupErr := temporaryWorkspace.Cleanup(); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("project inspect: clean temporary workspace: %w", cleanupErr))
		}
	}()

	project, err := snapshot.Resolve(path, snapshot.Boundaries{
		AuditOutput: outputBoundary,
		TempParent:  temporaryParent,
	})
	if err != nil {
		return result, err
	}
	identity, err := model.CollectProjectIdentity(project.Root())
	if err != nil {
		return result, err
	}
	destination := filepath.Join(temporaryWorkspace.Root(), "snapshot")
	if err := os.Mkdir(destination, 0o700); err != nil {
		return result, fmt.Errorf("project inspect: create snapshot destination: %w", err)
	}
	copied, err := project.Copy(destination)
	if err != nil {
		return result, err
	}
	result, _, err = buildProjectInspection(project.Root(), copied, identity)
	if err != nil {
		return result, err
	}
	if err := project.Verify(copied); err != nil {
		return result, fmt.Errorf("project inspect: source changed during inspection: %w", err)
	}
	if err := verifyProjectIdentity(project.Root(), identity); err != nil {
		return result, err
	}
	return result, nil
}

func verifyProjectIdentity(root string, expected model.ProjectIdentity) error {
	verified, err := model.CollectProjectIdentity(root)
	if err != nil {
		return err
	}
	if !sameProjectIdentity(expected, verified) {
		return errors.New("project inspect: Git marker changed during inspection")
	}
	return nil
}

func sameProjectIdentity(left, right model.ProjectIdentity) bool {
	return left.Name == right.Name && left.Git.Present == right.Git.Present &&
		sameOptionalString(left.Git.Head, right.Git.Head) && sameOptionalBool(left.Git.Dirty, right.Git.Dirty)
}

func sameOptionalString(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func sameOptionalBool(left, right *bool) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func writeProjectInspection(inspection passiveProjectInspection, requested string) (output string, err error) {
	parent := filepath.Dir(requested)
	base := filepath.Base(requested)
	if requested == "" {
		parent, _, err = ensureInspectionParent(inspection.root)
		if err != nil {
			return "", err
		}
		var identifier [16]byte
		if _, err := io.ReadFull(rand.Reader, identifier[:]); err != nil {
			return "", fmt.Errorf("generate inspection ID: %w", err)
		}
		base = "inspect-" + hex.EncodeToString(identifier[:])
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return "", fmt.Errorf("inspect output parent: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("inspect output parent is not a directory")
	}
	if base == "." || base == "" || base == string(filepath.Separator) {
		return "", errors.New("inspect output must name a new directory")
	}
	output = filepath.Join(parent, base)
	if _, err := os.Lstat(output); err == nil {
		return "", errors.New("inspect output already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect output: %w", err)
	}
	staging, err := os.MkdirTemp(parent, "."+base+".staging-")
	if err != nil {
		return "", fmt.Errorf("create inspection staging directory: %w", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(staging); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove inspection staging directory: %w", removeErr))
		}
	}()
	objects := []struct {
		name  string
		bytes []byte
	}{
		{name: "project-snapshot.json", bytes: inspection.project.SnapshotManifest().Bytes()},
		{name: "project-model.json", bytes: inspection.project.ProjectModel().Bytes()},
		{name: "sdk-coverage.json", bytes: inspection.coverage.SDKCoverage.Bytes()},
	}
	for _, object := range objects {
		if err := writeInspectionFile(filepath.Join(staging, object.name), object.bytes); err != nil {
			return "", err
		}
	}
	if err := syncInspectionDirectory(staging); err != nil {
		return "", fmt.Errorf("sync inspection staging directory: %w", err)
	}
	if err := publishInspectionDirectory(parent, filepath.Base(staging), base); err != nil {
		return "", fmt.Errorf("publish inspection directory: %w", err)
	}
	if err := syncInspectionDirectory(parent); err != nil {
		return "", fmt.Errorf("sync inspection output parent: %w", err)
	}
	return output, nil
}

func ensureInspectionParent(root string) (string, []string, error) {
	return ensureInspectionParentNamed(root, "inspect")
}

// ensureInspectionParentNamed creates .openbox/<leaf> under the project root,
// reporting which directories it created so a failed run can remove exactly
// those and leave a pre-existing tree alone.
func ensureInspectionParentNamed(root, leaf string) (string, []string, error) {
	current := root
	created := make([]string, 0, 2)
	for _, name := range []string{".openbox", leaf} {
		current = filepath.Join(current, name)
		if err := os.Mkdir(current, 0o700); err == nil {
			created = append(created, current)
		} else if !errors.Is(err, os.ErrExist) {
			return "", created, fmt.Errorf("create default inspection output: %w", err)
		}
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", created, errors.New("default inspection output parent is not a directory")
		}
	}
	return current, created, nil
}

func removeEmptyInspectionParents(paths []string) error {
	var result error
	for index := len(paths) - 1; index >= 0; index-- {
		if err := os.Remove(paths[index]); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove empty inspection output parent: %w", err))
		}
	}
	return result
}

func writeInspectionFile(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create inspection artifact: %w", err)
	}
	if written, writeErr := file.Write(content); writeErr != nil {
		err = writeErr
	} else if written != len(content) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write inspection artifact: %w", err)
	}
	return nil
}

func syncInspectionDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	err = directory.Sync()
	if closeErr := directory.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (a *app) parseProjectInspectArgs(args []string) (projectInspectOptions, int, bool) {
	options := projectInspectOptions{path: "."}
	fs := a.newFlagSet("openbox project inspect")
	fs.StringVar(&options.output, "output", "", "write exactly three local artifacts to DIR (default .openbox/inspect/<inspection-id>)")
	fs.Usage = func() {
		fmt.Fprint(a.stderr, projectUsage)
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, interspersedProjectInspectArgs(args)); !ok {
		return projectInspectOptions{}, code, false
	}
	if fs.NArg() > 1 {
		fmt.Fprint(a.stderr, projectUsage)
		return projectInspectOptions{}, a.errorf("project inspect accepts at most one path"), false
	}
	if fs.NArg() == 1 {
		options.path = fs.Arg(0)
	}
	if options.path == "" {
		return projectInspectOptions{}, a.errorf("project inspect path must not be empty"), false
	}
	if fs.Lookup("output").Value.String() == "" && flagWasSet(fs, "output") {
		return projectInspectOptions{}, a.errorf("project inspect --output must not be empty"), false
	}
	return options, exitOK, true
}

// interspersedProjectInspectArgs reorders the documented `PATH --output DIR`
// form into the flags-then-positionals order Go's flag package requires. A
// dangling --output returns the flags alone so the package reports its own
// "flag needs an argument": appending the "--" delimiter would otherwise be
// consumed as that flag's value.
func interspersedProjectInspectArgs(args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, 1)
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if strings.HasPrefix(argument, "-") && argument != "-" {
			flags = append(flags, argument)
			if argument == "--output" || argument == "-output" {
				if index+1 == len(args) {
					return flags
				}
				index++
				flags = append(flags, args[index])
			}
			continue
		}
		positionals = append(positionals, argument)
	}
	if len(positionals) == 0 {
		return flags
	}
	return append(append(flags, "--"), positionals...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(current *flag.Flag) {
		if current.Name == name {
			set = true
		}
	})
	return set
}
