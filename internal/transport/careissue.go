package transport

import "os"

// ReissueIfNeeded re-mints the CA in dir when it still carries the
// legacy name constraint (CANeedsReissue), and returns it unchanged
// otherwise. It is the one-time migration off a legacy constrained CA: delete
// both files, then LoadOrCreateCA regenerates an unconstrained pair under the
// same filenames (caCertFile/caKeyFile are constants), so NODE_EXTRA_CA_CERTS
// and every other reference built from CAPaths(dir) keeps pointing at the
// right place without being rewritten.
//
// Idempotent: a second call against a CA this function already reissued (or
// one that never needed it) finds CANeedsReissue false and touches no file,
// so a caller may run this on every `init` or `doctor` pass without checking
// first.
//
// This does not touch system trust or the PAC for the old CA -- swapping a
// running relay's CA mid-flight breaks in-flight handshakes and silently
// invalidates whatever the client already trusts -- so the caller decides
// when to call this (before the transport unit is next installed or
// restarted) and is responsible for untrusting the old fingerprint and
// trusting the new one under the same elevation prompt, recording both.
func ReissueIfNeeded(dir string) (*CA, error) {
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		return nil, err
	}
	if !CANeedsReissue(ca) {
		return ca, nil
	}
	certPath, keyPath := CAPaths(dir)
	if err := os.Remove(certPath); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return LoadOrCreateCA(dir)
}
