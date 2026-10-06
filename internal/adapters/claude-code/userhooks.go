package claudecode

import "path/filepath"

// UserSettingsPath is Claude Code's user-wide settings file, where an install
// registers its hooks so every session on this machine is governed regardless
// of the directory it starts in.
//
// Home is resolved exactly as userPluginDir resolves it, and that is
// load-bearing rather than incidental: the hook command written into this file
// names the engine inside the bundle under the same home, so two different
// answers would register a path that does not exist.
//
// This duplicates gatewayservice.SettingsPath by value on purpose. That
// package is CLI-layer and an adapter must not depend upward; the repo already
// keeps two copies of a path or a writer where the alternative is a wrong
// dependency.
func UserSettingsPath() string {
	return filepath.Join(homeDir(), ".claude", "settings.json")
}

// ProjectSettingsPath is a project's own settings file. One definition, so the
// sweep and the audit cannot address different files.
func ProjectSettingsPath(projectDir string) string {
	return filepath.Join(projectDir, ".claude", "settings.local.json")
}

// SweepProjectHooks takes OpenBox's registrations out of one project's
// settings file and reports what it removed. Every install runs it, because an
// install now governs every session on this machine: an entry surviving in a
// project file is a second registration of the same gate, and where it names a
// different engine path the tool will not de-duplicate the two.
//
// An absent file is a no-op, not an error. Most directories were never
// initialized, and creating a settings file in one would leave an OpenBox
// artifact in a repository that never asked for it.
func SweepProjectHooks(projectDir string) ([]string, error) {
	return RemoveLocalHooks(ProjectSettingsPath(projectDir))
}
