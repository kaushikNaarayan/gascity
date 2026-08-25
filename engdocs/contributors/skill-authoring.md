# Skill authoring

Create a skill only when it carries project-specific guidance that changes an
agent's decision. Keep the entrypoint short, route conditional detail to a
reference, and validate a new skill with the skill validator when available.

## Optional scope check

Before drafting a new skill or contributor guide, use this short dialogue:

1. **Context:** What durable project convention or repeated failure motivates it?
2. **Gap:** What decision would an agent make poorly without it?
3. **Pushback:** Is this better as code, a test, an existing skill update, or no change?
4. **Scope:** Name the intended trigger, non-goals, owner files, and verification.

Put the answers in the bead or review description when they affect scope. This
is a lightweight authoring aid, not a required ceremony: skip it for a small
correction to an existing instruction.
