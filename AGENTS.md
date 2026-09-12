<!-- SOL-LUNA-WORKFLOW:START -->
## Adaptive Sol–Luna coding workflow

For repository coding tasks, use the project-local `sol-luna-router` skill in `.codex/skills/sol-luna-router/SKILL.md` before substantial work.

The intended primary thread is GPT-5.6 Luna with `max` reasoning. Luna is the persistent default controller and implementer. Sol is a persistent independent GPT-5.6 Sol `high` project thread used only when stronger discovery, planning, or review is justified.

### Authority

The user's latest explicit instruction has higher workflow authority than Sol plans, Sol complexity labels, Luna routing judgments, and default workflow policy. LOW/MEDIUM/HIGH monotonicity constrains Luna only; the user may explicitly raise or lower complexity, force Luna or Sol, reorder/pause work, replace a design decision, or request replanning.

A user override may change only one work item or may invalidate part/all of a goal plan. Apply the smallest necessary scope. Never preserve an older Sol plan by reinterpreting a clear newer user instruction.

### Core routing

- LOW: Luna owns discovery, decisions, implementation, and verification.
- MEDIUM: Luna gathers bounded objective evidence; Sol owns consequential decisions/planning; Luna implements.
- HIGH: stop Luna-side discovery early; Sol owns discovery + planning; Luna implements; the same Sol thread reviews the result.
- Never invoke Sol merely because a task touches many files or takes many steps.
- Never silently escalate to Astra unless project settings explicitly enable it.
- For substantive multi-step goals, Sol owns decomposition and assigns LOW/MEDIUM/HIGH expected complexity to each work item.
- During goal execution, Luna may raise a Sol-assigned complexity when reality is harder than planned, but may not lower it unless the user explicitly overrides it or Sol revises the plan.
- If effective complexity rises or a material plan assumption fails, stop the affected branch and consult the same persistent Sol thread; Sol revises the versioned goal plan.
- If the user changes a local work item, do not trigger global replanning unless dependencies/invariants are affected. If the user changes a goal-level invariant, architecture choice, compatibility requirement, or other structural premise, mark the affected plan stale and replan unless the user explicitly supplies the replacement plan.

Read `.codex/sol-luna/POLICY.md` and `.codex/sol-luna/USER_CONTROL.md` for details.
<!-- SOL-LUNA-WORKFLOW:END -->

