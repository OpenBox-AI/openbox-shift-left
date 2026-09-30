package muse

import (
	"errors"

	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// errNotInstallable keeps `openbox init --provider muse` from half-installing
// while the hook handlers it would register do not exist yet.
var errNotInstallable = errors.New("muse: the installer is not built yet; nothing was written")

// Installer writes OpenBox's hook handlers into Muse's settings.json.
type Installer struct {
	// EngineBinary is the absolute path of the openbox binary the handlers run.
	EngineBinary string
}

// Name reports the provider this installer serves.
func (Installer) Name() providerspi.Name { return providerspi.Muse }

// Install refuses until the handlers exist.
func (Installer) Install(providerspi.CredentialRef) error { return errNotInstallable }

// HookInvocationMarkers are the substrings that identify an OpenBox
// registration in Muse's settings file.
func HookInvocationMarkers() []string { return []string{"hook muse "} }

// RemoveHooks takes every OpenBox registration out of Muse's settings file.
// Nothing can have been installed yet, so there is nothing to remove.
func RemoveHooks(settingsPath string) ([]string, error) { return nil, nil }
