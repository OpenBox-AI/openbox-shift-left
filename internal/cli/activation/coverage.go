package activation

import (
	"fmt"
	"maps"
	"slices"
)

// Coverage is one activated lane against what the tool's settings say now,
// because env routing is not durable against the tool that owns the file:
type Coverage struct {
	Lane    Lane
	Managed []string
	Missing []string
	// Changed are managed keys now holding another value: someone else owns them.
	Changed      []string
	SettingsPath string
}

func (c Coverage) Intact() bool { return len(c.Missing) == 0 && len(c.Changed) == 0 }

func (c Coverage) Vanished() bool {
	return len(c.Managed) > 0 && len(c.Missing) == len(c.Managed)
}

func (c Coverage) Describe() string {
	switch {
	case c.Vanished():
		return fmt.Sprintf("every key %s manages is gone from %s (%v). Something rewrote that file; "+
			"a running tool keeps its old environment, so this is invisible from inside a session",
			c.Lane, c.SettingsPath, c.Missing)
	case len(c.Missing) > 0 && len(c.Changed) > 0:
		return fmt.Sprintf("%s: %v missing from %s and %v now hold other values",
			c.Lane, c.Missing, c.SettingsPath, c.Changed)
	case len(c.Missing) > 0:
		return fmt.Sprintf("%s: %v missing from %s", c.Lane, c.Missing, c.SettingsPath)
	case len(c.Changed) > 0:
		return fmt.Sprintf("%s: %v in %s now hold values this lane did not write; something else "+
			"owns the routing", c.Lane, c.Changed, c.SettingsPath)
	}
	return ""
}

// CoverageOf compares each lane's managed keys against the settings as they are.
// It refuses an unreadable file: that is a different finding from keys missing.
func CoverageOf(homeDir string) ([]Coverage, error) {
	record, err := loadRecord(homeDir)
	if err != nil {
		return nil, err
	}

	var out []Coverage
	for _, lane := range lanePrecedence {
		entry := record.Lanes[lane]
		if entry == nil || len(entry.Managed) == 0 {
			continue
		}
		read := ReadSettingsEnv(entry.SettingsPath)
		if !read.Readable() {
			return nil, fmt.Errorf("activation: %s", read.Problem())
		}
		c := Coverage{
			Lane:         lane,
			Managed:      slices.Sorted(maps.Keys(entry.Managed)),
			SettingsPath: entry.SettingsPath,
		}
		for _, key := range c.Managed {
			want := entry.Managed[key]
			got, present := read.Env[key]
			switch {
			case !present:
				c.Missing = append(c.Missing, key)
			case got != want:
				c.Changed = append(c.Changed, key)
			}
		}
		out = append(out, c)
	}
	return out, nil
}
