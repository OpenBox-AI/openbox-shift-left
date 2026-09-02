//go:build !darwin && !linux

package runfs

func PublishDirectoryNoReplace(string, string) error { return ensureSupportedPlatform() }
