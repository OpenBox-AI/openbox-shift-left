//go:build darwin || linux

package snapshot

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

func selectEntries(project *Project) ([]Entry, error) {
	entries, _, err := selectEntriesDetailed(project, false)
	return entries, err
}

func selectEntriesWithDefaults(project *Project) ([]Entry, []omissionObservation, error) {
	return selectEntriesWithPolicy(project, false)
}

func selectEntriesWithPolicy(project *Project, dependencies bool) ([]Entry, []omissionObservation, error) {
	return selectEntriesDetailed(project, true, dependencies)
}

func selectEntriesDetailed(project *Project, defaults bool, dependencyMode ...bool) ([]Entry, []omissionObservation, error) {
	rootFD, err := unix.Open(project.root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot: open project root without following: %w", err)
	}
	root := os.NewFile(uintptr(rootFD), project.root)
	defer root.Close()
	rootInfo, err := root.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(project.identity, rootInfo) {
		return nil, nil, errors.New("snapshot: project root identity changed before selection")
	}
	device, err := directoryDevice(root)
	if err != nil {
		return nil, nil, err
	}
	entries := make([]Entry, 0)
	omissions := make([]omissionObservation, 0)
	dependencies := len(dependencyMode) == 1 && dependencyMode[0]
	if err := walkDirectory(project, root, device, "", defaults, dependencies, &entries, &omissions); err != nil {
		return nil, nil, err
	}
	current, err := os.Lstat(project.root)
	if err != nil || !os.SameFile(project.identity, current) {
		if err != nil {
			return nil, nil, fmt.Errorf("snapshot: stat project root after selection: %w", err)
		}
		return nil, nil, errors.New("snapshot: project root identity changed during selection")
	}
	return entries, omissions, nil
}

func walkDirectory(
	project *Project,
	directory *os.File,
	device uint64,
	relative string,
	defaults bool,
	dependencies bool,
	selected *[]Entry,
	omissions *[]omissionObservation,
) error {
	children, err := directory.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("snapshot: read %s: %w", displayPath(relative), err)
	}
	sort.Slice(children, func(left, right int) bool { return children[left].Name() < children[right].Name() })
	for _, child := range children {
		childRelative, err := normalizeRelative(path.Join(relative, child.Name()))
		if err != nil {
			return fmt.Errorf("snapshot: invalid source path under %s: %w", displayPath(relative), err)
		}
		match, excluded := project.matchBoundary(childRelative)
		if defaults {
			match, excluded = project.matchPathWithDependencies(childRelative, dependencies)
		}
		if excluded {
			*omissions = append(*omissions, omissionObservation{path: childRelative, match: match})
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(int(directory.Fd()), child.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("snapshot: stat %s without following: %w", childRelative, err)
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			childDirectory, openErr := openChildDirectory(directory, child.Name(), device)
			if errors.Is(openErr, unix.EXDEV) {
				appendSelected(
					Entry{Path: childRelative, Kind: KindExternalMount, Mode: fsMode(uint32(stat.Mode))},
					defaults, selected, omissions,
				)
				continue
			}
			if openErr != nil {
				return fmt.Errorf("snapshot: open directory %s without following: %w", childRelative, openErr)
			}
			walkErr := walkDirectory(project, childDirectory, device, childRelative, defaults, dependencies, selected, omissions)
			closeErr := childDirectory.Close()
			if walkErr != nil || closeErr != nil {
				return errors.Join(walkErr, closeErr)
			}
		case unix.S_IFREG:
			file, openErr := openRegularFile(directory, child.Name(), device)
			if errors.Is(openErr, unix.EXDEV) {
				appendSelected(
					Entry{Path: childRelative, Kind: KindExternalMount, Mode: fsMode(uint32(stat.Mode))},
					defaults, selected, omissions,
				)
				continue
			}
			if openErr != nil {
				return fmt.Errorf("snapshot: open regular file %s without crossing a mount: %w", childRelative, openErr)
			}
			var openedStat unix.Stat_t
			statErr := unix.Fstat(int(file.Fd()), &openedStat)
			closeErr := file.Close()
			if statErr != nil || closeErr != nil {
				return errors.Join(statErr, closeErr)
			}
			if openedStat.Mode&unix.S_IFMT != unix.S_IFREG {
				return fmt.Errorf("snapshot: regular file %s changed type while selecting", childRelative)
			}
			appendSelected(Entry{
				Path: childRelative,
				Kind: KindRegular,
				Mode: fsMode(uint32(openedStat.Mode)),
				Size: openedStat.Size,
			}, defaults, selected, omissions)
		case unix.S_IFLNK:
			entry, classifyErr := classifySymlink(
				project,
				directory,
				child.Name(),
				childRelative,
				fsMode(uint32(stat.Mode)),
				device,
			)
			if classifyErr != nil {
				return classifyErr
			}
			appendSelected(entry, defaults, selected, omissions)
		case unix.S_IFSOCK:
			appendSelected(Entry{Path: childRelative, Kind: KindSocket, Mode: fsMode(uint32(stat.Mode))}, defaults, selected, omissions)
		case unix.S_IFIFO:
			appendSelected(Entry{Path: childRelative, Kind: KindFIFO, Mode: fsMode(uint32(stat.Mode))}, defaults, selected, omissions)
		case unix.S_IFCHR, unix.S_IFBLK:
			appendSelected(Entry{Path: childRelative, Kind: KindDevice, Mode: fsMode(uint32(stat.Mode))}, defaults, selected, omissions)
		default:
			appendSelected(Entry{Path: childRelative, Kind: KindOther, Mode: fsMode(uint32(stat.Mode))}, defaults, selected, omissions)
		}
	}
	return nil
}

func appendSelected(entry Entry, defaults bool, selected *[]Entry, omissions *[]omissionObservation) {
	if defaults {
		if match, excluded := matchEntry(entry); excluded {
			*omissions = append(*omissions, omissionObservation{path: entry.Path, match: match})
			return
		}
	}
	*selected = append(*selected, entry)
}

func classifySymlink(
	project *Project,
	directory *os.File,
	name string,
	relative string,
	mode os.FileMode,
	device uint64,
) (Entry, error) {
	target, err := readlinkAt(directory, name)
	if err != nil {
		return Entry{}, fmt.Errorf("snapshot: read symlink %s: %w", relative, err)
	}
	candidate, inside, err := linkCandidate(project.root, path.Dir(relative), target, nil)
	if err != nil {
		return Entry{}, fmt.Errorf("snapshot: resolve symlink %s target: %w", relative, err)
	}
	if !inside {
		return Entry{Path: relative, Kind: KindExternalSymlink, Mode: mode}, nil
	}
	for hops := 0; hops < 64; hops++ {
		rootFD, openErr := unix.Open(project.root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		if openErr != nil {
			return Entry{}, fmt.Errorf("snapshot: reopen project root while resolving %s: %w", relative, openErr)
		}
		root := os.NewFile(uintptr(rootFD), project.root)
		rootInfo, statErr := root.Stat()
		if statErr != nil || !os.SameFile(project.identity, rootInfo) {
			_ = root.Close()
			if statErr != nil {
				return Entry{}, statErr
			}
			return Entry{}, errors.New("snapshot: project root identity changed during symlink resolution")
		}
		components := splitRelative(candidate)
		current := root
		prefix := ""
		followed := false
		for index, component := range components {
			var stat unix.Stat_t
			statErr := unix.Fstatat(int(current.Fd()), component, &stat, unix.AT_SYMLINK_NOFOLLOW)
			if errors.Is(statErr, os.ErrNotExist) || errors.Is(statErr, unix.ENOTDIR) {
				closeTraversal(root, current)
				return Entry{Path: relative, Kind: KindBrokenSymlink, Mode: mode}, nil
			}
			if statErr != nil {
				closeTraversal(root, current)
				return Entry{}, fmt.Errorf("snapshot: inspect symlink %s target: %w", relative, statErr)
			}
			if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
				next, readErr := readlinkAt(current, component)
				if readErr != nil {
					closeTraversal(root, current)
					return Entry{}, fmt.Errorf("snapshot: read symlink hop for %s: %w", relative, readErr)
				}
				candidate, inside, err = linkCandidate(project.root, prefix, next, components[index+1:])
				closeTraversal(root, current)
				if err != nil {
					return Entry{}, fmt.Errorf("snapshot: resolve symlink hop for %s: %w", relative, err)
				}
				if !inside {
					return Entry{Path: relative, Kind: KindExternalSymlink, Mode: mode}, nil
				}
				followed = true
				break
			}
			if index == len(components)-1 {
				if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
					child, childErr := openChildDirectory(current, component, device)
					if errors.Is(childErr, unix.EXDEV) {
						closeTraversal(root, current)
						return Entry{Path: relative, Kind: KindExternalSymlink, Mode: mode}, nil
					}
					if childErr != nil {
						closeTraversal(root, current)
						return Entry{}, fmt.Errorf("snapshot: open symlink %s directory target: %w", relative, childErr)
					}
					_ = child.Close()
				} else if stat.Mode&unix.S_IFMT == unix.S_IFREG {
					file, fileErr := openRegularFile(current, component, device)
					if errors.Is(fileErr, unix.EXDEV) {
						closeTraversal(root, current)
						return Entry{Path: relative, Kind: KindExternalSymlink, Mode: mode}, nil
					}
					if fileErr != nil {
						closeTraversal(root, current)
						return Entry{}, fmt.Errorf("snapshot: open symlink %s file target: %w", relative, fileErr)
					}
					var openedStat unix.Stat_t
					statErr := unix.Fstat(int(file.Fd()), &openedStat)
					closeErr := file.Close()
					if statErr != nil || closeErr != nil {
						closeTraversal(root, current)
						return Entry{}, errors.Join(statErr, closeErr)
					}
					if openedStat.Mode&unix.S_IFMT != unix.S_IFREG {
						closeTraversal(root, current)
						return Entry{}, fmt.Errorf("snapshot: symlink %s file target changed type while selecting", relative)
					}
				}
				continue
			}
			if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
				closeTraversal(root, current)
				return Entry{Path: relative, Kind: KindBrokenSymlink, Mode: mode}, nil
			}
			child, childErr := openChildDirectory(current, component, device)
			if errors.Is(childErr, unix.EXDEV) {
				closeTraversal(root, current)
				return Entry{Path: relative, Kind: KindExternalSymlink, Mode: mode}, nil
			}
			if childErr != nil {
				closeTraversal(root, current)
				return Entry{}, fmt.Errorf("snapshot: open symlink %s target component: %w", relative, childErr)
			}
			if current != root {
				_ = current.Close()
			}
			current = child
			prefix = path.Join(prefix, component)
		}
		if followed {
			continue
		}
		closeTraversal(root, current)
		normalized, normalizeErr := normalizeRelative(candidate)
		if normalizeErr != nil {
			return Entry{}, fmt.Errorf("snapshot: normalize symlink %s target: %w", relative, normalizeErr)
		}
		return Entry{Path: relative, Kind: KindInternalSymlink, Mode: mode, LinkTarget: normalized}, nil
	}
	return Entry{}, fmt.Errorf("snapshot: symlink %s exceeds 64 resolution hops", relative)
}

func linkCandidate(root, base, target string, remaining []string) (string, bool, error) {
	if filepath.IsAbs(target) {
		rootComponents := strings.Split(strings.Trim(filepath.ToSlash(filepath.Clean(root)), "/"), "/")
		if len(rootComponents) == 1 && rootComponents[0] == "" {
			rootComponents = nil
		}
		rootIndex := 0
		stack := make([]string, 0)
		seenTargetComponent := false
		for _, component := range strings.Split(filepath.ToSlash(target), "/") {
			if component == "" || component == "." {
				continue
			}
			if rootIndex < len(rootComponents) {
				if component == ".." || component != rootComponents[rootIndex] {
					return "", false, nil
				}
				rootIndex++
				continue
			}
			if component == ".." {
				if seenTargetComponent {
					return "", false, errors.New("absolute symlink target uses '..' after a path component")
				}
				if len(stack) == 0 {
					return "", false, nil
				}
				stack = stack[:len(stack)-1]
				continue
			}
			stack = append(stack, component)
			seenTargetComponent = true
		}
		if rootIndex != len(rootComponents) {
			return "", false, nil
		}
		stack = append(stack, remaining...)
		if len(stack) == 0 {
			return ".", true, nil
		}
		return strings.Join(stack, "/"), true, nil
	}
	stack := splitRelative(path.Clean(base))
	seenTargetComponent := false
	for _, component := range strings.Split(filepath.ToSlash(target), "/") {
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			if seenTargetComponent {
				return "", false, errors.New("relative symlink target uses '..' after a path component")
			}
			if len(stack) == 0 {
				return "", false, nil
			}
			stack = stack[:len(stack)-1]
			continue
		}
		stack = append(stack, component)
		seenTargetComponent = true
	}
	stack = append(stack, remaining...)
	if len(stack) == 0 {
		return ".", true, nil
	}
	return strings.Join(stack, "/"), true, nil
}

func readlinkAt(directory *os.File, name string) (string, error) {
	for size := 256; size <= 65536; size *= 2 {
		buffer := make([]byte, size)
		count, err := unix.Readlinkat(int(directory.Fd()), name, buffer)
		if err != nil {
			return "", err
		}
		if count < len(buffer) {
			return string(buffer[:count]), nil
		}
	}
	return "", errors.New("symlink target exceeds 65535 bytes")
}

func closeTraversal(root, current *os.File) {
	if current != root {
		_ = current.Close()
	}
	_ = root.Close()
}

func splitRelative(relative string) []string {
	if relative == "." {
		return nil
	}
	return strings.Split(filepath.Clean(relative), string(filepath.Separator))
}

func displayPath(relative string) string {
	if relative == "" {
		return "."
	}
	return relative
}

func fsMode(mode uint32) os.FileMode {
	return os.FileMode(mode & 0o777)
}

func directoryDevice(directory *os.File) (uint64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Dev), nil
}
