# stack

A minimal Go CLI (`stack`) and optional extension surface for personal AI agent orchestration across macOS (arm64), Linux (x64), and Windows (x64).

## Platforms

Develop on macOS or Linux. Windows (x64) is a supported runtime target, built and tested in CI (`windows-latest`), so `cmd/stack` and `internal/` must stay portable across OSes (no Unix-only syscalls; handle Windows paths such as `C:\Users\...` in `check public`). Scheduled `cycle` runs via `launchd` on macOS only, while `stack run cycle` itself works cross-platform.

## Commands

Go and development tools are pinned with mise (`mise.toml`, exact versions in `mise.lock`).

```sh
mise install
mise exec -- lefthook install
mise exec -- go build ./cmd/stack
mise exec -- scripts/test
```

`scripts/test` is the completion check of every change: `gofmt`, `golangci-lint`, and `go test -race ./...`. The hooks in `lefthook.yml` check formatting, workflows (`actionlint`, `ghalint`), and staged secrets (`betterleaks`) on commit, the commit message (`pommitlint`, Conventional Commits) on `commit-msg`, and run `scripts/test` on push.

## Architecture & Conventions

- `docs/architecture.md` is the canonical design specification (v10).
- **Minimal core (`cmd/stack`, `internal/`):** Zero runtime dependencies on `extensions/`. Must work cleanly with no extensions configured.
- **Hard safety boundaries only:** Only guard irreversible, near-zero-false-positive hazards (secrets read, local paths in public text, raw `gh pr merge`). Do not reintroduce over-defensive mechanisms (owner-only merge lists, `readonly_` live locks, self-defending budget ratchets, recovery state machines, or broad wolf-crying denies).
- **Skills live in `shuymn/dotfiles`:** Do not duplicate `home/dot_claude/skills/` here; this repo owns the execution CLI (`stack`), optional `extensions/`, and `--replay` `fixtures/`.
- Commit messages follow Conventional Commits.
- Pin every GitHub Action by full commit SHA with a version comment, and add any new action or reusable workflow to `selected_actions.patterns_allowed` in `.github/infra.yaml`.
- `mise.lock` is updated by `.github/workflows/mise-update.yml`, not by Renovate.
