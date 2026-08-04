package main

import (
	"errors"
	"flag"
)

// newFlagSet builds a subcommand's flag set with diagnostics on stderr, so the
// INV-3 stdout contract is never disturbed by usage text.
func (a *app) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	return fs
}

// parseFlags parses a subcommand's arguments and maps the outcome onto an exit
// code, so every command treats -h the same way.
//
// Asking for help is not an error: `openbox dev sync -h` exited 0 while
// `openbox init -h` exited 1, purely because only one call site checked for
// flag.ErrHelp. Scripts and CI notice that.
func parseFlags(fs *flag.FlagSet, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, false
		}
		return exitError, false
	}
	return exitOK, true
}
