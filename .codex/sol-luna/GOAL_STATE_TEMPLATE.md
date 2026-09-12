# Goal execution state

> Runtime copy: `.codex/sol-luna/.state/goal-state.md`. Keep this compact; it is recovery/control state, not a transcript.

## Original goal

<verbatim or faithful original user goal>

## Execution control

- State: <running | paused | stop_after_current | completed | blocked>
- User-owned decisions pending: <none or short list>
- Current execution boundary: <optional, e.g. stop after W4>

## Active user overrides

| ID | Scope | Directive | Effect on route/plan | Status |
|---|---|---|---|---|
| U1 | local:W4 | ... | W4 HIGH -> LOW; force Luna | active |

Keep only overrides that still affect execution. Status: `active`, `consumed`, `superseded`.

## Goal route

<LOW | MEDIUM | HIGH>

## Sol plan

- Plan version: <n>
- Plan status: <valid | partially_stale | stale | user_replaced>
- Last Sol sync: <timestamp / thread turn if useful>

## Goal-level invariants / acceptance

- <invariant>
- <acceptance criterion>

## Work items

| ID | Sol expected complexity | Effective complexity | Route authority | Status | Dependencies | Summary |
|---|---|---|---|---|---|---|
| W1 | LOW | LOW | Sol plan | pending | none | ... |

`Route authority` examples: `Sol plan`, `Luna upgrade`, `User override U1`, `Sol plan v3`.

Allowed status: `pending`, `ready`, `in_progress`, `blocked`, `done`, `needs_replan`, `skipped_by_user`.

## Current item

- ID: <...>
- Current effective complexity: <...>
- Current bounded objective: <...>

## New evidence / deviations since last Sol sync

- <only evidence that may affect plan/complexity>

## Unresolved decisions / blockers

- <none or item>

## Completion gate

- [ ] All user-required work items done or explicitly skipped/removed by user
- [ ] Goal-level acceptance criteria pass as currently defined
- [ ] No unresolved plan deviation or active structural override awaiting reconciliation
- [ ] Final Sol review passed when currently required (unless user explicitly waived it)
