package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// So we carry input_tokens / output_tokens / total_tokens directly and must
// not add the cache/reasoning sub-counts (adding would double-count).

const maxRolloutBytes = 64 << 20 // 64 MiB

// rolloutTokenUsage is the numbers-only projection of a Codex `TokenUsage`
// (codex-rs @ rust-v0.145.0).
type rolloutTokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	// CachedInputTokens / CacheWriteInputTokens are SUB-counts of InputTokens,
	// unlike Claude Code's cache counts which are additive siblings. Bound as of
	// contract v1.1 so they can be reported in their own fields; which means
	// Input must have them subtracted out to be pure input, not added to it.
	CachedInputTokens     int `json:"cached_input_tokens"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens"`
}

type rolloutTokenInfo struct {
	TotalTokenUsage *rolloutTokenUsage `json:"total_token_usage"`
}

type rolloutPayload struct {
	Info *rolloutTokenInfo `json:"info"`
	// Model is the ONE string this projection egresses, and it appears at exactly
	// this path: `turn_context.payload.model`.
	Model string `json:"model"`
}

type rolloutLine struct {
	Payload *rolloutPayload `json:"payload"`
}

// readRolloutUsage returns (nil, "", nil) when the rollout carries no token
// counts at all (a valid session that never recorded usage; the caller then
// attaches nothing, same as finops-off).
func readRolloutUsage(path string) (*client.Tokens, string, error) {
	if path == "" {
		return nil, "", fmt.Errorf("no transcript_path")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open rollout: %w", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return nil, "", fmt.Errorf("stat rollout: %w", err)
	} else if !fi.Mode().IsRegular() {
		return nil, "", fmt.Errorf("rollout is not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxRolloutBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read rollout: %w", err)
	}
	if len(raw) > maxRolloutBytes {
		return nil, "", fmt.Errorf("rollout exceeds %d-byte cap", maxRolloutBytes)
	}
	tokens, model := aggregateRolloutUsage(raw)
	return tokens, model, nil
}

// aggregateRolloutUsage empty when the rollout names none, in which case the
// pair is still emitted and the core-side extractor buckets it as unknown;
// never substituted from anywhere else.
func aggregateRolloutUsage(raw []byte) (*client.Tokens, string) {
	var latest rolloutTokenUsage
	var seen bool
	var model string

	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rl rolloutLine
		if err := json.Unmarshal(line, &rl); err != nil {
			continue
		}
		if rl.Payload == nil {
			continue
		}
		if rl.Payload.Model != "" {
			model = rl.Payload.Model // last non-empty turn_context wins
		}
		if rl.Payload.Info != nil && rl.Payload.Info.TotalTokenUsage != nil {
			latest = *rl.Payload.Info.TotalTokenUsage // last cumulative snapshot wins
			seen = true
		}
	}

	if !seen {
		return nil, model // valid, but nothing to report
	}

	rawIn := nonNegRollout(latest.InputTokens)
	cacheRead := nonNegRollout(latest.CachedInputTokens)
	cacheWrite := nonNegRollout(latest.CacheWriteInputTokens)
	in := nonNegRollout(rawIn - cacheRead - cacheWrite)
	out := nonNegRollout(latest.OutputTokens)
	total := nonNegRollout(latest.TotalTokens)
	// If a snapshot omitted it (0), fall back to the derived sum so Total is
	// never a spurious 0 while the parts are non-zero.
	if total == 0 {
		total = in + out + cacheRead + cacheWrite
	}
	tokens := &client.Tokens{
		Input:              intPtrRollout(in),
		Output:             intPtrRollout(out),
		CacheCreationInput: intPtrRollout(cacheWrite),
		CacheRead:          intPtrRollout(cacheRead),
		Total:              intPtrRollout(total),
	}
	return tokens, model
}

func intPtrRollout(v int) *int { return &v }

func nonNegRollout(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
