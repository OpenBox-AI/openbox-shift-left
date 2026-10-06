package muse

// SessionLogRoot is Muse's sessions directory as this process resolves it from
// $HOME. `openbox init` resolves it once and writes it into the telemetry unit,
// because the daemon that reads the journals has no $HOME of its own.
func SessionLogRoot() string { return sessionLogRoot() }
