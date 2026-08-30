//go:build windows

package gatewaycheck

// Windows has no uid to read, so statUID keeps its -1 default and tier detection
// degrades to "owner unknown".
//
// That is the deliberate direction: reporting the MDM tier without being able to
// observe ownership would claim an assurance level this build cannot check, which
// is the overstatement the package doc forbids. Windows daemon packaging is
// deferred anyway (phase 07 requirement 7) and the repo's posture there is
// build-verified only.
