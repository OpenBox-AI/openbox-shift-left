package activation

// This file is every OS besides darwin, today. The Linux and Windows command
// shapes (gsettings/certutil/pkexec on Linux, the registry/certutil/UAC on
// Windows) are unmeasured -- no probe has run against either OS -- so this
// arm asks for nothing, touches no Runner, and prints no manual command: a
// guessed command is worse than none, and "not attempted" is already the
// correct, honest report for an OS this build does not yet support.

func activateSystemPACUnsupported() Outcome {
	return Outcome{
		Class:  NotAttempted,
		Reason: "system PAC/CA activation is not implemented for this OS in this build",
	}
}

func deactivateSystemPACUnsupported() Report {
	return Report{
		Class:  NotAttempted,
		Reason: "system PAC/CA activation is not implemented for this OS in this build",
	}
}

func liveSystemPACUnsupported() []ScopeState { return nil }
