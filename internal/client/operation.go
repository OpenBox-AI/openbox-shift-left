package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Operation identity — what makes an approval survive a retry.
//
// A tool call has two identities, and conflating them broke the approval loop:
//
//	invocation — THIS attempt. Claude Code mints a fresh tool_use_id per call.
//	operation  — WHAT is being done. `ls -la` is the same operation whether it
//	             is the first attempt or the fourth.
//
// activity_id is derived from the OPERATION, because that is what an approval
// is about: an approver decides "may this be done", not "may this particular
// attempt be made". Core agrees — its own ComputeApprovalFingerprint keys on
// semantic identity (file path, MCP arg shape, a hash of the DB statement) for
// exactly this reason, and it scopes both bypass grants by activity_id on the
// assumption that a retry keeps it. That assumption holds for Temporal, where a
// retried activity keeps its id, and did not hold here.
//
// The discriminator is HASHED, and the hash never becomes a wire field of its
// own — it is folded into activity_id, which is already an opaque
// `cc-act-<sha256 prefix>`. So this adds precision without adding a content
// field and without changing the egress posture (INV-2). It is a correlation
// id, not a secret: a short command drawn from a small space could in principle
// be confirmed by an offline guess against the id, by a party that already
// holds the session id and the tool name.

// operationHashLen bounds the hex digest folded into the key. 16 hex chars (8
// bytes) is ample for distinguishing operations within one session, which is
// the whole scope an activity_id is compared in.
const operationHashLen = 16

// OperationForCommand is the operation identity of a shell tool call: the
// command it runs. Two different commands must never share an approval —
// approving `ls` must not grant `rm -rf /` — and the same command retried must
// share one.
//
// The command itself is local-only and never egressed (INV-2); only this hash
// influences the id.
func OperationForCommand(command string) string {
	if command == "" {
		return ""
	}
	return "cmd:" + shortHash([]byte(command))
}

// OperationForArgs is the operation identity of an MCP tool call: the argument
// shape, hashed. The server and function are already structural fields of the
// key, so this is what distinguishes one call of a tool from another.
//
// It mirrors core's own rule, stated in ComputeApprovalFingerprint: "same tool
// with different arguments must require fresh approval". Without it, approving
// one `create_issue` would grant every later `create_issue` in the session.
//
// Arguments are canonicalized before hashing so that a semantically identical
// payload with different key order or whitespace yields the same id — otherwise
// a retry could differ for no reason a human would recognize. An unparseable
// input is hashed verbatim: it is still deterministic, which is all the id
// needs.
func OperationForArgs(rawArgs []byte) string {
	if len(rawArgs) == 0 {
		return ""
	}
	if canonical, err := canonicalJSON(rawArgs); err == nil {
		return "args:" + shortHash(canonical)
	}
	return "args:" + shortHash(rawArgs)
}

// canonicalJSON re-marshals a document so key order and insignificant
// whitespace cannot change the hash (Go emits map keys sorted).
func canonicalJSON(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:operationHashLen]
}
