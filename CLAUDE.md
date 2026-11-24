# toomanyclients

Claude Code skill and Go CLI that reads Go source and rendered Kubernetes manifests, adds up the Postgres connections every pod's pools can open (database/sql `SetMaxOpenConns`, pgxpool `MaxConns` and `pool_max_conns`) at full scale and during a rolling update, and compares the total with `max_connections` minus reserved slots.

## Architecture
All code is in `plugin/scripts`, one `package main` with no dependencies, so `go run` and `run.sh` need no setup. `scan.go` finds files and refuses broken input, `eval.go` groups packages and evaluates integer expressions (constants, env vars, flags, struct tags, helpers), `model.go` finds pools and programs, `k8s.go` reads workloads, autoscalers, ConfigMaps and Postgres operator objects, `pgconf.go` reads `postgresql.conf`, `check.go` matches programs to containers and builds the budget and findings, and `main.go` has flags, output and exit codes. `yaml_*.go` is a copy of go.yaml.in/yaml/v3: don't edit it. See `docs/architecture.md`.

`examples/` is a separate Go module that needs pgx. `examples/repro` runs real pools against a real Postgres; it isn't part of `make test`.

## Commands
- `make test`: gofmt, vet, unit tests and integration tests in `tests/`, with `-race`
- `make lint`: plugin and marketplace validation
- `make demo`: run the examples
- `make repro`: run pools against a real Postgres (network, Go 1.23+, `DATABASE_URL`)

## Conventions
- Standard library only. Never build, run or connect to what is being checked.
- Input that can't be trusted (empty, binary, cut off, failed command output, nothing to compare) exits 3 with no result. Never print a clean result when anything was assumed or unreadable.
- A value that can't be read is a TC9xx warning, never a guess. Where a guess is unavoidable (several values for one setting), take the highest.
- Each finding has a stable `TCnnn` code. Don't reuse codes.
- A claim about what Postgres, Kubernetes, database/sql or pgx does must come from its docs or source, linked in the pull request, and ideally from a run of `examples/repro`.
- Tests must not assume the machine they run on.

## Do and don't
- Do add a test for what a change counts and for what it must not count, and keep `TestEveryCut` and `TestBrokenFilesEachSide` passing.
- Don't guess which database a program uses or which workload runs it beyond the name rules in `docs/architecture.md`.

## Definition of done
`make test`, `make lint` and `make demo` pass, and `README.md`, `ROADMAP.md` and `CHANGELOG.md` are updated when relevant.
