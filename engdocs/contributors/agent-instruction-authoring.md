---
title: Authoring Agent Instructions
description: Keep SKILL.md and AGENTS.md instructions easy for agents to find and act on.
---

## Context load and cognitive load

Before merging a new or edited `SKILL.md` or `AGENTS.md`, make a context-load
and cognitive-load pass. **Context load** is the always-present text that
consumes tokens and attention; **cognitive load** is the effort required to
find the applicable instruction among competing guidance. Keep universally
needed steps near the task, move conditional reference behind a precisely
worded pointer, and prefer familiar leading words over newly coined jargon.

Run the **no-op test** sentence by sentence: would removing this instruction
change the model's behavior from its default? If not, delete it rather than
compressing it. Preserve instructions that encode project-specific facts,
safety constraints, or non-obvious completion criteria. This is a required
review pass, not a reason to remove guidance without checking its behavior.

The source lens is [Writing for Agents](https://github.com/mattpocock/skills/blob/main/skills/productivity/writing-for-agents/SKILL.md).
