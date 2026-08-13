# mattpocock ecosystem → Gas City integration eval

Survey of `github.com/mattpocock` (Matt Pocock — TypeScript/AI-dev educator,
Total TypeScript, AI Hero) plus his `ai-hero-dev` org, for reusable patterns
in how we AUTHOR Claude Code Skills, agent/prompt tooling, and developer
CLIs. Same treatment as the kunchenguid ecosystem eval
(`docs/kunchenguid-integration-roadmap.md` in the town's `city` scaffold, not
this repo) — same shape: rubric, ranked roadmap table, classification verbs,
winners-where-how, SKIP rationale, and an Antithesis-style honest
counterpoint for every rejection. Read via `gh api`, not WebFetch — WebFetch
runs a small model that summarizes and drops items.

Enumeration source: `gh api users/mattpocock/repos?per_page=100` (2 pages,
~160 repos) + `gh api orgs/ai-hero-dev/repos`. `total-typescript` org is
private/404 to us. Non-fork, non-course-scaffold repos with plausible
transfer to a Go/gc/skills stack were deep-dived; pure TS-teaching content
(react-typescript-tutorial-*, total-typescript-book-*, pro-essentials-*,
etc.) was excluded from scoring — wrong domain, no agent/dev-tooling
content.

## Rubric (skills lens, /20)

Scored across 6 axes, ~3-4 pts each: token cost of the pattern at runtime,
description/trigger quality (does it fire when it should), progressive
disclosure (thin top file + deep references), reusability across
rigs/projects, maintenance burden, safety/egress. For non-skill repos
(orchestration libraries, CLIs) the trigger-quality axis is substituted with
CLI/API ergonomics, noted inline.

## Classification scheme

- **INTEGRATE** — adopt the tool itself as a pack/skill.
- **PORT** — build the mechanism natively (Go, our own skill/hook), don't
  shell out to someone else's binary/runtime.
- **ADOPT-PRACTICE** — a cross-cutting authoring/design principle to bake
  into how we write skills, docs, and formulas — no tool to install, but
  concrete beads/edits.
- **SKIP** — wrong domain, or a worse version of something we already have.

## Ranked roadmap (cheapest / highest-value-per-effort first)

| # | Repo / pattern | Score/20 | Class | Impact | Effort | Follow-on bead |
|---|---|---|---|---|---|---|
| 1 | **git-guardrails PreToolUse hook** (from `mattpocock/skills`) | n/a | PORT | Turns our prose git-safety rules (AGENTS.md: no force-push, no `checkout -- .`, etc.) into a mechanically enforced hook instead of an instruction an agent can ignore | S | `gcf-7nb` |
| 2 | **Context-load vs cognitive-load authoring lens** (from `mattpocock/skills`, `writing-for-agents/SKILL.md`) | 17/20 | ADOPT-PRACTICE | Concrete rewrite lens + a "no-op test" for pruning our own SKILL.md/AGENTS.md instructions | S | `gcf-20d` |
| 3 | **4-section docs-page template sourced from real questions** (from `mattpocock/skills`, `.agents/writing-docs.md`) | — | ADOPT-PRACTICE | Sharper Mintlify docs pages (`docs/`) under `gascity-docs` house style | S | `gcf-vd6` |
| 4 | **Ralph-pack reference implementation + explicit budget-cap gap** (from `mattpocock/ralph-workshop-repo-001/002`, `ai-hero-dev/ai-hero-cli:plans/ralph.sh`) | 13-14/20 | ADOPT-PRACTICE | Gas City's own docs name-check "Ralph" as a peer orchestration pack but have no reference config; this closes that with an explicit cost-governance guardrail the upstream examples lack | S–M | `gcf-4e0` |
| 5 | **Idle-timeout + completion-grace-window race** (from `mattpocock/sandcastle`, `src/Orchestrator.ts`) | 18/20 | ADOPT-PRACTICE | Concrete state-machine reference for a session that emits a completion signal but doesn't exit — relevant to health-patrol/session-idle logic | S | `gcf-7za` |
| 6 | **`what-to-test` skill template + scope.md pre-authoring dialogue** (from `ai-hero-dev/ai-hero-cli`, `ai-hero-dev/cohort-003-skill-building`) | 12-14/20 | ADOPT-PRACTICE | Terse test-scope skill for `cmd/gc` ("if a user would notice it's broken, it needs a test") + a lightweight self-interrogation scoping step before writing new skills/docs | S | `gcf-d8g` |
| — | `mattpocock/sandcastle` (as a whole library) | 18/20 | SKIP (as INTEGRATE/PORT target) | High build quality but a single-agent iterate-until-signal loop with no DAG/gate/dispatch — strictly narrower than Gas City's existing dispatcher | — | idea → #5 |
| — | `mattpocock/evalite` | 8/20 | SKIP | TS-native LLM eval framework; domain-orthogonal to Go orchestration, no code to port | — | note only |
| — | `mattpocock/graph-docs-cli` | 9/20 | SKIP | Narrower than our own `graphify` skill (explicit-frontmatter DAG only, no semantic clustering); `graphify` supersedes it | — | note only |
| — | `mattpocock/slopwatch` | 6/20 | SKIP | Mostly unbuilt stub; vocabulary contract (Session→Turn→Model-request DAG) worth emulating later, nothing to port today | — | revisit if built |
| — | `mattpocock/mcp-server-from-prompts` | 4/20 | SKIP | Toy demo; Gas City explicitly bans MCP tool registration | — | — |
| — | `mattpocock/dictionary-of-ai-coding` | 3/20 | SKIP | Pure glossary content, zero architecture | — | — |
| — | `mattpocock/matt-cli` | 2/20 | SKIP | Personal video-production scripts, zero transfer | — | — |
| — | ~150 TS-teaching / course-scaffold repos | — | SKIP | Wrong domain (React/TS pedagogy, boilerplates, personal sites) | — | — |

## Winners — WHERE + HOW

### 1. git-guardrails PreToolUse hook — PORT

`mattpocock/skills`, `skills/misc/git-guardrails-claude-code/SKILL.md` +
`scripts/block-dangerous-git.sh`. A `PreToolUse` hook that pattern-matches
`tool_input.command` against `git push` (non-`--dry-run`), `reset --hard`,
`clean -f`, `branch -D`, and pathspec `checkout <ref> -- .`, returning exit
code 2 to hard-block the tool call. This is the mechanical enforcement of
rules our own AGENTS.md already states in prose ("Never run `git checkout
<ref> -- .`", "Never force-push", "no destructive git ops without asking")
— currently trust-based, not enforced. Port the pattern-match list as a
`.claude/hooks/` (or town-wide settings.json hook) PreToolUse script, tuned
to our own destructive-command list (add `bd delete --force`, `tmux
kill-server` per our existing house rules).

### 2. Context-load vs cognitive-load lens — ADOPT-PRACTICE

`mattpocock/skills`, `skills/productivity/writing-for-agents/SKILL.md`. A
documented theory distinguishing "context load" (tokens in the window) from
"cognitive load" (how hard it is for the model to find the relevant
instruction among competing ones), plus two concrete techniques: "leading
words" (reuse pretrained concepts as compact tokens instead of coining new
jargon) and a "no-op test" for pruning instructions — does removing this
line change model behavior versus its default, or is it dead weight? Apply
this lens the next time we edit `AGENTS.md` or any `SKILL.md` — it's a
rewrite checklist, not new infrastructure.

### 3. 4-section docs-page template — ADOPT-PRACTICE

`mattpocock/skills`, `.agents/writing-docs.md`. Every promoted skill gets an
external docs page built from a fixed 4-section template — **What it does /
When to reach for it / Common questions / It's working if** — and the
"common questions" section is sourced from real user questions (GitHub
issues, wiki), not invented ones. Apply this template shape to new
`docs/` (Mintlify) pages under the `gascity-docs` skill's house style; source
the FAQ section from actual `bd` mail/escalation history instead of
guessing likely questions.

### 4. Ralph-pack reference + budget-cap gap — ADOPT-PRACTICE

`mattpocock/ralph-workshop-repo-001` and `-002` (`plans/afk-claude.sh`,
`plans/prompt.md`) plus `ai-hero-dev/ai-hero-cli` (`plans/ralph.sh`, a
working non-workshop copy of the same pattern). The "Ralph" pattern: a bash
loop invokes a coding CLI once per iteration with a PRD + the last N
`RALPH:`-prefixed git commit messages as its only memory, and stops on a
literal `<promise>COMPLETE</promise>` (or `NO MORE TASKS`/`ABORT`) string in
the model's output. Gas City's own AGENTS.md names "Ralph" as a peer
orchestration pack alongside Gas Town — but there's no reference config
documenting what a "Ralph pack" on Gas City would look like. Both upstream
implementations have **no cost/iteration budget ceiling beyond the raw
iteration count** and no time cap — a genuine gap given our cost-governance
policy (quota-guard, nightly shutdown). Write a short reference doc: a
Ralph-shaped formula/order config using Gas City's own bead-backed state
(instead of grepping git log) with health-patrol's existing budget/hold
primitives layered on top as the explicit fix for the upstream gap.

### 5. Idle-timeout + completion-grace-window race — ADOPT-PRACTICE

`mattpocock/sandcastle`, `src/Orchestrator.ts` +
`docs/adr/0019-completion-timeout-for-hanging-process.md`. When an agent
process emits its completion signal but doesn't actually exit (e.g. a child
process holding stdout open), sandcastle starts a short grace timer and
force-completes the run using the buffered output instead of hanging
indefinitely or failing the whole run. This is a clean state-machine
reference for a scenario our own health-patrol / session-idle-detection
logic can hit (an agent session that signals done but the tmux/runtime
process lingers) — worth a comparison read against
`internal/session/lifecycle_projection.go` and the health-patrol stall
detection to confirm we already handle this race, or file the gap.

### 6. `what-to-test` skill + scope.md pre-authoring dialogue — ADOPT-PRACTICE

`ai-hero-dev/ai-hero-cli`, `.claude/skills/what-to-test/SKILL.md`: a terse
test-scope skill — "Test user-facing behavior. If a user would notice it's
broken, it needs a test," with an explicit `/* v8 ignore start */` policy
for DI-wiring/integration boundaries. Worth a native `cmd/gc` equivalent
skill defining what warrants a unit test vs. what's acceptable to leave to
integration coverage, tuned to our own `TESTING.md` tier boundaries.
Separately, `ai-hero-dev/cohort-003-skill-building`,
`exercises/04-day-2-steering/04.04-what-are-agent-skills/scope.md`: a
recorded self-interrogation dialogue (context snapshot → gap analysis →
pushback → locked scope) run *before* drafting a lesson/skill. Adopt as a
lightweight pre-writing step for new Gas City skills/docs — mirrors what
`materiallab-learning-loop` should already be doing for recurring friction,
extended to new-skill authorship.

## SKIP rationale (one-liners)

- **`mattpocock/sandcastle`** (7379★, INTEGRATE/PORT target) → SKIP as a
  library: a single-agent iterate-until-signal loop with no DAG, dependency,
  gate, or fan-out primitive — strictly narrower than Gas City's own
  worktree-isolated dispatcher. **Revisit if:** Gas City ever wants an
  interactive "keep nudging one agent until done" mode distinct from the
  molecule/dispatch model — `Orchestrator.ts`'s timeout state machine is a
  clean reference to reimplement natively.
- **`mattpocock/evalite`** (1652★) → SKIP: TS-native LLM eval framework,
  domain-orthogonal to a Go orchestration SDK, no code worth porting.
  **Revisit if:** Gas City wants a bead-native "eval formula" scoring agent
  output quality — its `--threshold=N` CI-gate ergonomic is worth imitating
  structurally at that point.
- **`mattpocock/graph-docs-cli`** (110★) → SKIP: narrower than our own
  `graphify` skill (explicit-frontmatter DAG + topo-sort only, no semantic
  clustering, needs a system `graphviz` binary). **Revisit if:** `graphify`
  ever needs a "minimum learning path through N docs" mode — its
  transitive-reduction + topo-sort (`src/getPathTo.ts`, ~30 lines) is worth
  re-deriving, not porting.
- **`mattpocock/slopwatch`** (44★) → SKIP: almost entirely unbuilt stub code
  (the Claude Code listener is a literal one-line placeholder). **Revisit
  if:** it matures — its Session→Turn→Model-request vocabulary contract
  (`CONTEXT.md`) is a good shape to emulate for our own event/session
  taxonomy discipline.
- **`mattpocock/mcp-server-from-prompts`** (12★) → SKIP: a toy demo of
  "write incremental prompts to scaffold an MCP server"; Gas City bans MCP
  tool registration outright ("if a tool has a CLI, the agent uses it").
- **`mattpocock/dictionary-of-ai-coding`** (3672★) → SKIP: generated
  glossary content, zero architecture or code to transfer.
- **`mattpocock/matt-cli`** (21★) → SKIP: personal video/course-production
  scripts (OBS trims, YouTube downloads), zero transfer.
- **`mattpocock/ai-hero-cli`, `mattpocock/mcp-server-from-prompts`'s
  parent-repo AGENTS.md** → largely converges with our own AGENTS.md
  conventions (same `bd` idiom, same non-interactive-shell guidance) —
  independent validation we're already doing this well, nothing to port.
- **~150 TS-teaching / course-scaffold / personal-site repos**
  (`react-typescript-tutorial-*`, `total-typescript-book-*`,
  `pro-essentials-workshop-*`, `gatsby-*`, etc.) → SKIP as a batch, wrong
  domain (React/TypeScript pedagogy content, boilerplates unrelated to
  agent/skill/CLI authoring).

## Safety/egress notes

- `mattpocock/skills`'s `setup-matt-pocock-skills`, `triage`, and
  `to-tickets` skills write AI-generated comments directly to GitHub/Linear
  issues — real third-party write-egress if a pack copied that content
  uncritically into an automated context. Not relevant to the winners above
  (none of them touch this).
- No telemetry/phone-home found in any deep-dived repo's install
  scripts/plugin manifests. `sandcastle`'s Vercel Firecracker sandbox
  provider is explicitly opt-in cloud; default execution is local
  Docker/Podman.
- The Ralph loop's only cost bound is the raw iteration count CLI arg — no
  token/dollar/time cap in either upstream implementation. Flagged
  explicitly in winner #4 as the gap our port must close.

## Follow-on beads

- `gcf-7nb` PORT git-guardrails PreToolUse hook (roadmap #1)
- `gcf-20d` ADOPT-PRACTICE context/cognitive-load authoring lens (roadmap #2)
- `gcf-vd6` ADOPT-PRACTICE 4-section docs-page template (roadmap #3)
- `gcf-4e0` ADOPT-PRACTICE Ralph-pack reference + budget-cap guardrail (roadmap #4)
- `gcf-7za` ADOPT-PRACTICE idle-timeout/completion-grace race review (roadmap #5)
- `gcf-d8g` ADOPT-PRACTICE `what-to-test` skill + scope.md pre-authoring dialogue (roadmap #6)

All filed OPEN and UNROUTED (no assignee, no `gc.routed_to`) — the mayor
triages and dispatches from here, per the eval bead's guardrails.
