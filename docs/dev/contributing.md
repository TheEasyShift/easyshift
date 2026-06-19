# Contributing (developer guide)

The short version lives in the root **[CONTRIBUTING.md](../../CONTRIBUTING.md)**.
This is the full workflow.

## Setup

See **[../user/installation.md](../user/installation.md)** for prerequisites and
building. For development you need Go 1.25+; the linters are pinned and run via
`go run`, so nothing else to install.

## The development loop

1. Read **[architecture.md](architecture.md)** first — the layering rules
   (`config ← interfaces ← {stages,providers} ← app ← cmd`) are load-bearing,
   and PRs that violate them will be hard to merge.
2. Make your change in the right layer:
   - New capability → interface in `interfaces/`, implementation in
     `providers/`, fake in `providers/fakes/`. See [providers.md](providers.md).
   - New install step → a package under `stages/`, wired into
     `app/manager.go:buildStages`. See [stages.md](stages.md).
3. Add tests with fakes. See [testing.md](testing.md).
4. Run the gate:
   ```sh
   make check        # vet + gofmt + build + test
   ```

## Layering rules (enforced by review)

- Stages import only `config` and `interfaces` — never `providers`, never
  another stage.
- Providers import only `config` and `interfaces` — never another provider,
  never a stage.
- `app` is the only package that imports concrete providers and stage packages.
- `config` has no behavior and imports no other internal package.

Keeping these intact is what makes the dependency graph acyclic and the stages
unit-testable.

## Conventions

- **String-valued flags over booleans** when more variants are foreseeable
  (`--magic-dns`, `--network-mode`, `--dns-provider`), so new options don't
  require new flags or breaking changes.
- **Narrow interfaces** — a stage should depend on the smallest surface it needs.
- **Idempotent `Apply`, best-effort `Rollback`** — installs resume and delete
  rolls back, so both must tolerate partial state.

## Commits

This repo has a specific commit convention, defined authoritatively in the root
**`CLAUDE.md`**:

- **Sign off every commit**: `git commit -s`.
- **Do not** use a `Co-Authored-By` trailer. When a change is assisted by an AI
  tool, end the message with:
  ```
  Assisted-by: Claude Code/<model-id>
  ```
  where `<model-id>` is the exact model that assisted (e.g.
  `claude-opus-4-7`).
- Keep commits focused and explain the **why** in the body, not just the what.

Example:

```
Make the NAT network a single shared global resource

NAT clusters previously each got their own libvirt network, so VMs in
different clusters couldn't reach each other. Treat the NAT network as
global state shared by all NAT clusters...

Assisted-by: Claude Code/claude-opus-4-7
```

## Pull requests

- Branch from `main`.
- Ensure `make check` is green before pushing.
- Describe the change and its motivation; note any user-facing flag/behavior
  changes so the [user docs](../user/) can be updated in the same PR.
- Keep doc changes alongside the code change that motivates them.
