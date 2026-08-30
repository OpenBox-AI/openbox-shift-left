package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const operationHashLen = 16

// OperationForCommand is the operation identity of a shell tool call: the
// command it runs. Two different commands must never share an approval;
// approving `ls` must not grant `rm -rf /`; and the same command retried must
// share one.
func OperationForCommand(command string) string {
	if command == "" {
		return ""
	}
	return "cmd:" + shortHash([]byte(command))
}

// OperationForArgs is the operation identity of an MCP tool call: the argument
// shape, hashed. It mirrors core's own rule, stated in
// ComputeApprovalFingerprint: "same tool with different arguments must require
// fresh approval".
func OperationForArgs(rawArgs []byte) string {
	if len(rawArgs) == 0 {
		return ""
	}
	if canonical, err := canonicalJSON(rawArgs); err == nil {
		return "args:" + shortHash(canonical)
	}
	return "args:" + shortHash(rawArgs)
}

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
