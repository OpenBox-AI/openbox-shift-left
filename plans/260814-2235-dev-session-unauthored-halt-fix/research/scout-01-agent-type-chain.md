# Scout: agent_type="developer" chain — VERIFIED end to end

Main-agent verification (2026-08-14) closing researcher-01 unresolved Q1+Q2.

## Chain (every hop read from source)

1. **shift-left sends it at registration.** `const developerAgentType = "developer"` —
   `cli/internal/devinit/devinit.go:43` ("free-form agent_type; no migration"); wire field
   `AgentType string \`json:"agent_type"\`` on the register request — `cli/internal/backend/client.go:87`
   (echoed in responses at `:111`, `:158`). 4xx on rejection is a named stop condition
   (`devinit.go:329-331`), so an org that rejects the field halts loudly, not silently.
2. **Backend accepts + persists.** openbox-backend `src/modules/agent/dto/create-agent.dto.ts:43`
   (`agent_type?: string`), entity `src/modules/agent/entities/agent.entity.ts:42`.
3. **Core reads it back for free.** `FindByToken` selects the full bob model, no column projection —
   openbox-core `internal/datastore/agent_pgx.go:33-46`; `AgentBobToRaw` maps
   `AgentType: v.AgentType.Ptr()` — `internal/datastore/bob_pgx.go:86,96`.
4. **In scope at both rejection blocks.** `agent *content.Agent` fetched at
   `governance_workflow.go:153-157` (step 1), before Block 1 (`:232-254`) and Block 2 (`:273-299`)
   — per researcher-01 Q1.

## Consequence for the fix

Discriminator: `agent.AgentType != nil && *agent.AgentType == "developer"`. Server-derived
(registration-time, backend-persisted), not client-asserted per event. No new query, no schema change.

## Residual caveat

Dev agents registered by flows predating `openbox auth` may have NULL `agent_type`. Safe default:
nil/other ⇒ agent-runtime semantics (status-quo latch behavior). Direction of error only ever
keeps the old bug for unmarked agents; never loosens agent-runtime governance. Remedy for such
agents: re-register (or backend-side update), out of plan scope.

## Unresolved questions

None for the discriminator. Attested-half semantics remain a phase-02 design point (researcher-01 Q3/Q4).
