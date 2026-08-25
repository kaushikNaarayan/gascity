---
name: what-to-test
description: Choose the smallest meaningful Gas City test for a cmd/gc or Go change. Use when deciding unit, coordination, integration, or documentation coverage.
---

# What to test

Test behavior a user or operator would notice. Start with the regression risk,
then place one proof at the smallest layer that owns it; `TESTING.md` is the
authoritative tier policy.

- A pure decision, validation branch, or domain transition belongs in a nearby
  unit test.
- CLI parsing, output, or exit status belongs in a fast CLI/testscript proof.
- Component argument plumbing belongs in one focused coordination test.
- Real processes, filesystems, stores, providers, or protocols retain one
  integration proof for that boundary.
- Generated documentation or schema agreement belongs in its existing
  doc-sync/freshness test.

Do not add coverage solely for dependency-injection wiring or trivial wrappers
when the exercised behavior already has an owning test. Record why the boundary
is intentionally covered elsewhere in the review or bead, rather than adding a
language-specific ignore directive.

Run the focused owner first. Before handoff, run the fast baseline and use the
sharded targets documented in `TESTING.md` for broader process or integration
coverage.
