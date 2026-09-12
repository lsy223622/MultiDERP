# User control and intervention protocol

The user remains the highest workflow authority. Sol is the senior engineering consultant; Luna is the controller/implementer; neither may preserve its own prior plan against a newer explicit user instruction.

This protocol changes workflow routing only. It does not bypass platform safety rules, authorization boundaries, or an explicit project constraint that the user has not changed.

## What the user may override

At any point, the user may explicitly:

- force Luna to execute a task without Sol consultation;
- force consultation or full discovery by Sol;
- raise or lower LOW/MEDIUM/HIGH complexity;
- accept, reject, replace, or constrain a Sol design decision;
- add, remove, split, merge, or reorder goal work items;
- change dependencies, implementation boundaries, invariants, or acceptance criteria;
- request a fresh goal decomposition/replan;
- pause autonomous execution, stop after the current safe unit, or resume;
- reserve a decision for the user instead of either model.

Natural-language intent is enough. No special command syntax is required.

## Precedence

For workflow decisions, apply this precedence:

1. latest explicit user instruction;
2. active user-defined project/goal constraints;
3. latest valid Sol plan/decision;
4. Sol complexity assignment;
5. Luna runtime complexity judgment;
6. default workflow policy.

If two user instructions conflict, the newer specific instruction controls for the scope it clearly addresses.

## Local vs structural override

Use the smallest scope consistent with the instruction.

### Local override

Affects one bounded work item without changing shared assumptions or later dependencies.

Examples:

- "W4 can be LOW; Luna should do it directly."
- "Ask Sol about this one decision only."
- "Do W7 before W6; they are independent."

Apply it to that item, record it in goal state when Goal Mode is active, and continue. Do not replan the entire goal merely because a local route changed.

### Structural override

Changes an assumption used by multiple work items or changes the goal itself.

Examples:

- breaking compatibility is now allowed;
- the authentication model should use a different ownership boundary;
- remove a previously required subsystem;
- add a new goal-level security or migration invariant.

Mark the affected plan portion stale. Normally send a `GoalReplan` request to the persistent Sol thread so it can issue a new `PLAN_VERSION` and reclassify affected work items. If the user explicitly provides the replacement decomposition/plan or explicitly says not to involve Sol, follow that instruction instead.

## In-flight intervention

When a new user message changes active work:

1. stop at the next safe execution boundary;
2. do not start new work that conflicts with the new instruction;
3. preserve completed valid work unless the user asks to revert it;
4. classify the override as local or structural;
5. update goal state if Goal Mode is active;
6. consult Sol only when the revised route requires it or the user asks for it;
7. resume from the revised state.

If an already-running external command cannot be safely interrupted, let that command return, report material side effects if any, then apply the override before the next step.

## Complexity override semantics

Without a user override, Sol's expected complexity is a floor and Luna may only raise it.

With an explicit user override:

- the user may lower or raise the effective complexity;
- Luna must not secretly restore the previous higher route "for safety" or quota policy;
- Luna may briefly state a material residual engineering risk, then follow the override;
- if new facts later contradict the user's stated assumptions, surface those facts and re-evaluate only to the extent permitted by the user's latest instruction.

## Pause and resume

If the user says to pause/stop after a boundary, finish only the smallest safe current unit, update recovery state, and stop autonomous execution. Do not continue later work until the user resumes or gives a replacement instruction.

On resume, re-read the latest goal state, active overrides, repository state, and any intervening user instructions before continuing.
