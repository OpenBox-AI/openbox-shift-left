//go:build !darwin && !linux

package snapshot

import "errors"

func selectEntries(_ *Project) ([]Entry, error) {
	return nil, errors.New("snapshot: safe no-follow source selection is not supported on this platform")
}

func selectEntriesWithDefaults(_ *Project) ([]Entry, []omissionObservation, error) {
	return nil, nil, errors.New("snapshot: safe no-follow source selection is not supported on this platform")
}

func selectEntriesWithPolicy(_ *Project, _ bool) ([]Entry, []omissionObservation, error) {
	return nil, nil, errors.New("snapshot: safe no-follow source selection is not supported on this platform")
}
