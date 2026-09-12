# Sol–Luna routing policy

## Authority model

The user's latest explicit instruction is the highest workflow authority. It may override routing, complexity, Sol plans, execution order, acceptance criteria, or whether Sol is involved at all.

The normal precedence is:

1. latest explicit user instruction;
2. active user-defined project/goal constraints;
3. latest valid Sol plan/decision;
4. Sol expected complexity;
5. Luna runtime complexity judgment;
6. default workflow policy.

The monotonic-complexity rule constrains Luna, not the user. See `USER_CONTROL.md`.

## Task Mode vs Goal Mode

Use **Task Mode** for one bounded task or a small set of largely independent, directly executable changes.

Use **Goal Mode** when the user specifies an outcome requiring sustained autonomous execution, non-trivial decomposition, dependency management, multiple milestones, or plan revision over time.

Do not enter Goal Mode merely because a prompt contains several bullets. Do not stay in Task Mode when decomposition itself is consequential. The user may explicitly force either mode.

## Core ownership model

LOW:
- Luna owns discovery, decisions, implementation, and verification.

MEDIUM:
- Luna owns bounded evidence gathering.
- Sol owns consequential design decisions and implementation planning.
- Luna owns implementation and routine repair.

HIGH:
- Luna owns only classification and transport until a bounded plan exists.
- Sol owns discovery, hypothesis formation, architectural reasoning, risk analysis, and planning.
- Luna owns implementation after Sol hands back a plan.
- Sol reviews the resulting implementation before final completion unless the user explicitly changes that requirement.

EXTREME:
- Use only after Sol is blocked, repeatedly wrong, or the task genuinely requires a stronger end-to-end agent.
- Do not spend Astra quota automatically unless project settings explicitly opt in or the user explicitly requests it.

## Classification rule

Classify by **decision difficulty and discovery uncertainty**, not by number of files or lines changed.

### LOW

Use Luna directly when the intended implementation is already clear and mistakes are easy to detect or reverse.

Typical examples:
- localized bug with known cause;
- mechanical refactor;
- rename/migration with an explicit rule;
- adding a field or endpoint by an established pattern;
- tests, lint, formatting, docs;
- implementation from an already-approved detailed plan.

Large code volume alone does not make a task MEDIUM/HIGH.

### MEDIUM

Use MEDIUM when consequential decisions exist, but Luna can gather the required facts without deciding which facts matter to the final diagnosis.

Typical examples:
- feature spanning several modules under an understood architecture;
- ordinary refactor with a few viable designs;
- adding lifecycle behavior where existing ownership is clear;
- compatibility-sensitive change with known compatibility surface;
- task where Sol should choose the approach, but repository reconnaissance is mostly mechanical.

The MEDIUM handoff must be an **evidence package**, not a proposed solution, unless the user has already mandated the solution.

### HIGH

Escalate directly to HIGH when discovery itself is part of the hard reasoning problem.

Hard triggers include:
- unknown root cause or intermittent failure;
- architecture redesign or unclear ownership boundaries;
- authentication, authorization, security boundary, cryptography;
- protocol semantics or distributed-system behavior;
- concurrency, races, deadlocks, cancellation/lifecycle ambiguity;
- data/schema migration with meaningful rollback/compatibility risk;
- multiple core subsystems with unclear interaction;
- several plausible hypotheses where deciding what to inspect is itself difficult;
- a MEDIUM task reveals hidden coupling that invalidates the initial evidence boundary;
- two focused Luna attempts fail without a clear mechanical reason.

For HIGH, do not pre-filter the repository for Sol. Preserve the raw task and let Sol decide what to inspect.

## Dynamic reclassification

Without a user override, classification is not permanent but is monotonic from Luna's perspective:

- LOW -> MEDIUM/HIGH when implementation uncovers consequential ambiguity.
- MEDIUM -> HIGH when evidence gathering reveals that deciding what evidence matters is itself hard.
- HIGH -> implementation once Sol has reduced the problem to a bounded plan.
- Luna must not lower a Sol-assigned MEDIUM/HIGH route on its own.

The user may explicitly raise or lower any route. Sol may lower a prior Sol assignment in a later plan/checkpoint.

## Review policy

- LOW: Luna verification is sufficient unless the user requests stronger review.
- MEDIUM: Sol review is normally unnecessary after the agreed plan is implemented and validation passes.
- HIGH: send the final diff/state back to the same persistent Sol thread for review unless the user explicitly waives or changes this.
- Prefer one focused correction loop. If a correction exposes a new architectural problem, transfer ownership back to Sol rather than letting Luna improvise.

## Goal-mode policy

A **goal** is an outcome that requires multiple dependent work items or a sustained autonomous execution loop. Goal complexity and work-item complexity are separate. A HIGH goal may contain many LOW work items.

### Who decomposes a goal

- Luna may decompose only goals that are obviously a small collection of LOW/mechanical tasks.
- Substantive goals are decomposed by the persistent Sol thread.
- MEDIUM goal: Luna may first gather bounded objective repository evidence; Sol owns decomposition and consequential planning.
- HIGH goal: Sol owns discovery, decomposition, risk analysis, and the initial plan directly from the original goal/current repository.
- An explicit user-provided decomposition may replace Sol decomposition. Route each user-defined work item according to the user's stated constraints and this policy.

### Required Sol work-item metadata

Every Sol-authored goal plan must give each work item:

- stable ID;
- objective;
- `expected_complexity: LOW | MEDIUM | HIGH`;
- dependencies;
- bounded implementation guidance;
- acceptance/verification criteria;
- assumptions/invariants;
- explicit triggers for re-consulting Sol.

### Complexity during execution

Absent a user override, Sol's work-item complexity is the initial floor. Luna can raise complexity but cannot silently lower it.

`effective_complexity = max(Sol expected complexity, Luna observed required complexity)` until Sol or the user explicitly revises the route.

- LOW: Luna owns execution and verification.
- MEDIUM: Luna may validate assumptions and gather bounded evidence. If the existing Sol plan already resolves all consequential choices and remains valid, Luna may execute. If a new consequential choice is required, consult the same Sol thread before continuing.
- HIGH: Sol temporarily owns task-specific discovery/decision work and returns a fresh or amended implementation packet before Luna proceeds.

### Plan invalidation

Luna must stop the affected execution branch and return to Sol when, absent a contrary user instruction:

- a material plan assumption is false;
- newly discovered coupling changes dependency/order or architecture;
- an acceptance criterion cannot be met under the current plan;
- the task's effective complexity rises;
- implementation would require changing an invariant or making an unplanned consequential decision.

Sol increments the goal plan version and identifies which completed/pending items are affected. Luna does not silently rewrite the overall decomposition.

### User intervention during a goal

Classify an override by scope:

- **Local**: only one bounded work item/route/order changes and shared assumptions remain valid. Apply it locally; do not replan the whole goal.
- **Structural**: goal-level requirement, invariant, architecture choice, compatibility/security premise, dependency graph, or multiple downstream items change. Mark the affected plan `partially_stale` or `stale` and normally request `GoalReplan` from the persistent Sol thread.

If the user explicitly provides the replacement plan or explicitly says not to consult Sol, follow that instruction instead.

Record active overrides and route authority in `.codex/sol-luna/.state/goal-state.md`. The latest explicit user instruction wins over an older Sol plan.

### Pause/resume

If the user pauses autonomous execution, stop at the next safe boundary, persist compact recovery state, and do not continue later work until a resume/replacement instruction arrives.

On resume, re-read goal state, active overrides, current repository state, and newer user instructions before continuing.

### Completion

A goal is complete only when all currently required work items are complete (or explicitly removed/skipped by the user), current goal-level acceptance criteria pass, and no unresolved plan deviation remains.

Goals initially HIGH, or containing any currently HIGH work item, normally require a final goal-level review by the same persistent Sol thread unless the user explicitly waives or replaces that requirement.
