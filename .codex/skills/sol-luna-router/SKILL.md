---
name: sol-luna-router
description: Route repository work between a Luna Max primary thread and a persistent independent Sol High project thread, for both bounded tasks and autonomous multi-step goals, with explicit user override/control.
---

# Sol–Luna Router

Apply this workflow to substantive repository work.

Read:

- `.codex/sol-luna/POLICY.md`
- `.codex/sol-luna/USER_CONTROL.md`
- `.codex/sol-luna/SOL_CONTRACT.md` when preparing Sol handoffs

## 0. Authority: user control comes first

The user's latest explicit instruction is the highest workflow authority.

The user may at any time force Luna or Sol, raise/lower complexity, replace a Sol decision, change work-item order/scope/acceptance criteria, pause/resume autonomous work, or request replanning.

The normal rule "Sol complexity is a floor" constrains **Luna only**. It does not constrain the user.

When a user intervention arrives during work:

1. stop at the next safe execution boundary;
2. do not begin new work that conflicts with it;
3. classify it as **local** or **structural**;
4. apply the smallest necessary scope;
5. update Goal State when Goal Mode is active;
6. consult Sol only if the revised route requires it or the user asks for it;
7. continue or pause according to the user's instruction.

Never reinterpret a clear newer user instruction merely to preserve an older Sol plan.

## 1. Non-negotiable architecture

- The primary long-lived thread is Luna Max: controller, router, implementer.
- Sol is a **persistent independent project thread**, not a normal spawned subagent.
- Never use `multi_agent_v1__spawn_agent`, `spawnAgent`, `spawn_agent`, or `fork_thread` for the persistent Sol consultant.
- Prefer the existing project Sol thread over creating another one.
- Sol quota is spent on difficult discovery, consequential decisions/planning, and risk-focused review—not routine implementation.
- Do not invoke Sol merely because work is large, touches many files, or takes many steps.

## 2. Select Task Mode or Goal Mode

Use **Task Mode** for one bounded task or a small set of directly executable/mostly independent changes.

Use **Goal Mode** when the user gives an outcome requiring non-trivial decomposition, dependent milestones, sustained autonomous execution, or plan revision over time.

Do not treat every multi-bullet prompt as Goal Mode. Do not keep a genuinely architectural multi-stage objective in Task Mode merely to avoid planning.

The user may explicitly force either mode.

# Task Mode

## 3. Classify before substantial work

Classify LOW / MEDIUM / HIGH using `POLICY.md`. Do this cheaply.

Do not inspect half the repository just to decide whether a task is HIGH. If a HIGH hard trigger is present, stop Luna-side discovery and transfer discovery ownership to Sol.

An explicit user route override controls instead of the automatic classification.

## 4. LOW route

Luna works directly:

- inspect only what is needed;
- implement;
- run proportionate verification;
- finish without contacting Sol.

If consequential ambiguity emerges, raise complexity dynamically unless the user's current instruction explicitly constrains the route.

## 5. MEDIUM route

Luna first gathers bounded, objective evidence.

Use this handoff shape:

```text
MODE: MEDIUM

ORIGINAL TASK
<preserve the user's actual request faithfully>

ACTIVE USER OVERRIDES / CONSTRAINTS
<latest explicit directives that affect this task, or none>

OBSERVED FACTS
- path:line — fact
- path:line — fact

CURRENT CONSTRAINTS / INVARIANTS
- only confirmed constraints

DECISIONS NEEDED FROM SOL
- concrete consequential questions

UNKNOWN OR CONTRADICTORY EVIDENCE
- preserve uncertainty; do not silently filter it out
```

Do not include Luna's preferred solution unless the user explicitly mandated it. Separate facts from interpretations.

Send to the persistent Sol thread. Luna implements the returned plan.

If a material Sol assumption proves false or implementation requires a new consequential decision, stop and consult the same Sol thread again—unless the user has explicitly taken ownership of that decision or forced a different route.

## 6. HIGH route

Do not perform Luna-led reconnaissance beyond what is needed to recognize HIGH complexity.

Use this handoff shape:

```text
MODE: HIGH

ORIGINAL TASK
<user request as directly and completely as possible>

ACTIVE USER OVERRIDES / CONSTRAINTS
<latest explicit directives that affect this task, or none>

USER-PROVIDED / ALREADY-CONFIRMED BACKGROUND
<facts known before Luna-side investigation>

CURRENT WORKTREE NOTE
<branch/uncommitted state only when material>

INSTRUCTION
Own discovery and planning. Inspect the repository yourself. Do not assume Luna's framing identifies the relevant subsystem or root cause.
```

Sol owns discovery + planning. Luna implements the bounded plan.

After implementation and verification, normally send the same Sol thread a HIGH review unless the user explicitly waived/replaced this requirement:

```text
MODE: REVIEW

CURRENT USER REQUIREMENT / ACTIVE OVERRIDES
<include any newer user directives>

The HIGH-complexity implementation is ready for review.
Inspect the actual current diff/files yourself.
Verify against the current user requirement, latest valid plan, invariants, and acceptance criteria.
Return PASS, FIX, or BLOCKED per SOL_CONTRACT.md.

IMPLEMENTATION NOTES
<short factual summary>

VERIFICATION RUN
<commands/results>
```

For `FIX`, Luna applies one focused correction and re-verifies. If the correction exposes new discovery/architecture work, return ownership to Sol instead of improvising.

# Goal Mode

## 7. Goal triage and decomposition

For a substantive multi-step goal, do **not** let Luna independently invent the decomposition unless the user has already supplied one or the goal is obviously just a small bundle of LOW/mechanical tasks.

- Trivial all-LOW goal: Luna may decompose and execute.
- MEDIUM goal: Luna may provide bounded objective reconnaissance; persistent Sol owns decomposition/planning.
- HIGH goal: give Sol the substantially unfiltered original goal; Sol owns discovery + decomposition + planning.
- User-provided decomposition: preserve it unless the user asks Sol to redesign it; Sol may still be consulted within individual work items according to route.

A Sol-authored goal plan must provide for every work item:

- stable ID;
- objective;
- `EXPECTED_COMPLEXITY: LOW | MEDIUM | HIGH`;
- dependencies;
- implementation guidance;
- acceptance/verification criteria;
- assumptions/invariants;
- explicit re-consult triggers.

It must also provide `PLAN_VERSION`, goal-level invariants/acceptance criteria, dependency/parallelism information, and global invalidation triggers.

## 8. Goal execution loop

Maintain `.codex/sol-luna/.state/goal-state.md` from `GOAL_STATE_TEMPLATE.md` for long autonomous runs.

Before each work item:

1. re-read active user overrides and execution-control state;
2. ensure dependencies are satisfied;
3. start with Sol's expected complexity unless a user override controls;
4. let Luna raise complexity if new evidence warrants it;
5. route by current **effective complexity**.

Absent a user override:

```text
effective complexity = max(Sol expected complexity,
                           Luna observed required complexity)
```

The user may explicitly lower or raise the effective route.

### LOW work item

Luna executes and verifies directly.

### MEDIUM work item

If the existing Sol plan has already resolved all consequential choices and its assumptions still hold, Luna may execute it without redundantly calling Sol again.

If a new consequential choice appears, send a bounded MEDIUM checkpoint/evidence package to the same Sol thread before continuing.

### HIGH work item

Before substantial implementation, return task-specific ownership to the same Sol thread. Sol refreshes relevant repository state, performs needed discovery, and returns a fresh/amended implementation packet. Luna implements it.

## 9. Complexity changes and plan invalidation

Luna may raise:

- LOW -> MEDIUM/HIGH
- MEDIUM -> HIGH

Luna may not lower a Sol-assigned route on its own. A lower route requires either:

- explicit user override; or
- explicit Sol reclassification in a later plan/checkpoint.

Stop the affected branch when a material plan assumption is false, dependencies/architecture change, an acceptance criterion is impossible under the plan, effective complexity rises, or an unplanned consequential decision is required.

Use a GoalCheckpoint for execution discoveries. Sol updates the plan version when necessary rather than Luna silently rewriting architecture.

## 10. User intervention during Goal Mode

### Local override

If the user's change affects only one bounded item and shared assumptions remain valid:

- record it under `Active user overrides`;
- update that item's `Effective complexity` and `Route authority`;
- execute the revised route;
- do **not** trigger global replanning merely because a local route changed.

Examples: force W4 to Luna; ask Sol only for W6; reorder two independent items; skip one optional work item.

### Structural override

If the user's change alters the goal, a goal-level invariant, architecture/compatibility/security premise, dependency graph, or multiple downstream items:

- mark affected plan state `partially_stale` or `stale`;
- pause affected branches;
- normally send `GoalReplan` to the persistent Sol thread with the user directive preserved verbatim/faithfully;
- let Sol increment `PLAN_VERSION`, preserve still-valid completed work, and reclassify only affected items.

If the user explicitly supplies the replacement plan or says not to involve Sol, follow that instruction instead.

### Pause / stop / resume

If the user says pause or stop after a boundary:

- finish only the smallest safe current unit;
- set goal execution state accordingly;
- persist recovery-critical state;
- stop autonomous execution.

On resume, re-read goal state, active overrides, current repository state, and newer user messages before continuing.

## 11. Goal completion gate

A passing current work item is not enough.

Finish the goal only when:

- all currently user-required work items are done or explicitly removed/skipped;
- current goal-level acceptance criteria pass;
- no unresolved deviation or stale plan portion remains;
- required final Sol review passes, unless the user explicitly waived/replaced it.

Goals initially HIGH or containing any currently HIGH item normally receive a final goal-level review from the same persistent Sol thread.

# Persistent Sol channel

## 12. Preferred: Codex App independent thread tools

When the current task exposes the complete Codex App lifecycle, use it directly only within the task-creation boundary below. A complete lifecycle means that the current tool registry provides all of `list_projects`, `list_threads`, `create_thread`, `send_message_to_thread`, and either `wait_threads` or `read_thread`; the presence of `list_threads` alone is not enough to declare the App path available for creating a new thread.

`create_thread` creates a user-visible task. The skill being installed or automatically selected is not, by itself, authorization to create one. Use `create_thread` only when the current user explicitly requests a new independent Sol task/session. Reusing an already-existing project Sol thread with `list_threads` and `send_message_to_thread` does not create a new task. If the user has not given that explicit authorization, use the CLI fallback for a new persistent consultant instead.

When App creation is authorized and the pointer is absent, follow this sequence:

1. Call `list_projects` and resolve the current repository's project ID, host ID, and Git status.
2. Call `list_threads` and reuse the existing top-level project thread titled approximately `Sol Architect — <repository-name>` when one exists.
3. If it does not exist, call `create_thread` with the resolved project, `gpt-5.6-sol`, `high`, and the repository's normal project environment. Do not use `multi_agent_v1__spawn_agent`, `spawnAgent`, `spawn_agent`, or `fork_thread` as a substitute.
4. If creation returns a `clientThreadId`, do not pass it to another thread tool. Poll `list_threads` until the corresponding real `threadId` is available, or treat native creation as unavailable after a bounded wait.
5. Send the handoff with `send_message_to_thread`, then use `wait_threads` or `read_thread` to obtain the result. The receiving thread must be the resolved Sol thread, not the current Luna task.
6. When possible, record the resolved App pointer in `.codex/sol-luna/.state/sol-thread.json` with `transport: "codex_app"`, `thread_id`, `host_id`, model, effort, and timestamps. Never pass an App pointer to `invoke-sol.ps1`.

- Reuse a project-local top-level thread titled approximately `Sol Architect — <repository-name>`.
- It must not be a spawned child/subagent thread.
- Create with `gpt-5.6-sol` + `high` when the actual tool schema supports explicit selection.
- Ensure it targets the current repository/project.
- On first turn, tell it to read `SOL_CONTRACT.md`, `USER_CONTROL.md`, `PROJECT_CONTEXT.md`, and `DECISIONS.md`.
- Preserve it for future tasks/goals.
- Treat `.codex/sol-luna/.state/sol-thread.json` as the canonical local pointer and record the resolved thread ID/model/effort/timestamps when possible.

Do not invent unavailable tool fields. Follow the running Codex tool schema.

## 13. Fallback: persistent CLI session

Use this for a new persistent consultant when the user has not authorized a new user-visible App task, or when the native App lifecycle is genuinely unavailable, incomplete after the bounded creation check, or cannot target the project. If the fallback reports a lock or another transport error, fail closed and report the blocker; do not retry blindly and do not start any ordinary subagent.

1. write the handoff to `.codex/sol-luna/.state/handoff.md`;
2. run:

```powershell
powershell -ExecutionPolicy Bypass -File ".codex/skills/sol-luna-router/scripts/invoke-sol.ps1" `
  -Mode <Medium|High|Review|GoalPlan|GoalCheckpoint|GoalReplan|GoalReview> `
  -InputFile ".codex/sol-luna/.state/handoff.md"
```

3. the script creates the Sol session once, stores its thread ID with `transport: "cli"`, and uses `codex exec resume` thereafter;
4. read returned text or `.codex/sol-luna/last-sol-response.md`.

Never replace this with a fresh `codex exec` for every consultation; persistence is part of the design.

For diagnostics during long goals, `scripts/show-workflow-state.ps1` may be used to display the Sol pointer and compact Goal State. It is read-only; user steering remains the control source.

## 14. Escalation beyond Sol

Do not automatically invoke Astra unless `.codex/sol-luna/settings.json` enables it or the user explicitly requests it.

If Sol remains blocked or repeatedly wrong, surface the exact escalation reason. Preserve its accumulated thread context.

## 15. Context hygiene

- Repository files are the source of truth.
- Sol's persistent conversation owns rich architectural/reasoning history.
- `PROJECT_CONTEXT.md` and `DECISIONS.md` hold compact durable facts/decisions, not transcripts.
- `goal-state.md` is compact execution/control recovery state, not a log.
- MEDIUM should not make Sol re-read the whole repository when objective evidence is sufficient.
- HIGH should not give Sol a Luna-filtered view of the repository.
- Always propagate active user overrides to Sol when they affect the consultation.
