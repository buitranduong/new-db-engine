# STASK: instructions for Claude sessions (read before doing anything)

STASK is an experimental tiered database engine in Go (log as the single source of truth, derived columnar/search/history views, PostgreSQL wire compatibility). It exists to replace a PostgreSQL + CDC + 2x Elasticsearch + Superset-copy stack for a SIM-card warehouse/CRM/web shop. Do not ask the user to repeat context: it is in the docs.

1. Read, in order: `docs/00_START_HERE.md`, `docs/STATUS.md`, `docs/AGENT_PROTOCOL.md`, `docs/PROCESS.md`, then `docs/14_plan_target_actual.md`.
2. Work only through branches/worktrees (`git worktree add ~/wt/<name> -b <name>`). Only the orchestrator merges into `main` and pushes.
3. Build binaries with `go build -o bin-main/ ./cmd/...` (never trust `bin/`). `go build ./...` does not write binaries.
4. A task is "done" only with reproducible evidence (tests, `cmd/claimcheck`, `cmd/pgdiff`, or measurements); fill your row in `docs/14`. Use `scripts/merge-gate.sh` before merging.
5. Do not critique other agents' designs; record findings in your report. Design decisions live in `docs/PROCESS.md`.
6. Shared 8-core VM, about 57 GB disk: check `df -h /` first (stop below 3 GB free), use your own ports/containers, delete only what you created, with real stop conditions. If an action is denied, do not work around it; report it.
7. Never change an on-disk format silently (see `docs/09_spec.md`): stop and report.
8. Docs are in Vietnamese; README is English. Keep docs honest: say what is not proven.
