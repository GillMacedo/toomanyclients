# toomanyclients: check that Go services' Postgres pools fit max_connections across Kubernetes pods

[![CI](https://github.com/GillMacedo/toomanyclients/actions/workflows/ci.yml/badge.svg)](https://github.com/GillMacedo/toomanyclients/actions/workflows/ci.yml) [![MIT license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**A Claude Code skill and Go CLI that adds up the Postgres connections your Go programs can open across their Kubernetes pods, at the HPA maximum and during a rolling update, and compares the total with `max_connections`, before pods fail with `FATAL: sorry, too many clients already`.**

Every pod has its own pool. A `SetMaxOpenConns(20)` that is fine for 3 replicas is 160 connections when the HPA scales to 8, and 200 while a rollout runs 2 extra pods. Postgres keeps 3 of its 100 default slots for superusers, so an application role gets 97. A database/sql pool with no `SetMaxOpenConns` has no limit at all, and a pgxpool on its default size opens as many connections as the node has CPUs, which a container's CPU limit doesn't change. None of this shows up in code review, in short tests or on a quiet day: it fails under load.

`toomanyclients` reads the Go source and the rendered manifests, finds each pool and where its size comes from (a constant, an env var, a ConfigMap, a flag or a DSN), multiplies by the pods that run it, and compares with what Postgres accepts. It never connects to a database or a cluster.

## What you can ask Claude

- "Our pods log 'sorry, too many clients already' since we raised the HPA max. Which pools add up to too many?"
- "Can we scale the api Deployment to 12 replicas without running out of Postgres connections?"
- "We use pgxpool.New with default settings. How many connections does each pod open?"
- "What pool size should each service use so everything fits in max_connections = 200, also during a deploy?"

## Install

As a Claude Code plugin:

    claude plugin marketplace add GillMacedo/toomanyclients
    claude plugin install toomanyclients@toomanyclients-marketplace

The skill builds the tool from source with Go 1.22 or newer. It has no dependencies. To run it yourself, clone the repo and use `go run ./plugin/scripts`, or `sh plugin/scripts/run.sh` from any folder.

## Example

`examples/oversubscribed` is a small shop. `api` uses database/sql with `SetMaxOpenConns` from the `DB_MAX_OPEN_CONNS` env var, which its Deployment sets to 20, and an HPA scales it to 8 pods. `worker` uses `pgxpool.New` with a DSN from a ConfigMap. The database is a CloudNativePG `Cluster` with `max_connections: "100"`:

    $ go run ./plugin/scripts --fail-on never examples/oversubscribed
    toomanyclients: 2 programs (api, worker) in 2 workloads, against 97 available connections (Cluster shop-db (CloudNativePG) at examples/oversubscribed/deploy/postgres.yaml:1)

    Budget
      Deployment api runs api: 8 pods (HorizontalPodAutoscaler api max) x 20 per pod, +2 pods in a rollout
        sql.Open at examples/oversubscribed/internal/store/store.go:24: SetMaxOpenConns(20) at examples/oversubscribed/internal/store/store.go:28 (DB_MAX_OPEN_CONNS=20 in Deployment api)
      Deployment worker runs worker: 2 pods (replicas) x >=4 per pod, +1 pod in a rollout
        pgxpool.New at examples/oversubscribed/cmd/worker/main.go:15: default MaxConns: the larger of 4 and the node's CPU count (DSN in DATABASE_URL from ConfigMap worker-config at examples/oversubscribed/deploy/worker.yaml:23 has no pool_max_conns)
      Total: at least 168 at full scale, at least 212 during a rolling update
      Postgres: max_connections 100 - superuser_reserved_connections 3 = 97

    ERROR TC101 examples/oversubscribed/deploy/api.yaml:21  At full scale the pods can open at least 168 connections, and Postgres accepts 97
      Deployment api (examples/oversubscribed/deploy/api.yaml:21) runs api: 8 pods (HorizontalPodAutoscaler api max) x 20 = 160; a rollout adds 2 pods
      Deployment worker (examples/oversubscribed/deploy/worker.yaml:1) runs worker: 2 pods (replicas) x at least 4 = 8; a rollout adds 1 pod
      Postgres: max_connections 100 - superuser_reserved_connections 3 = 97 (from Cluster shop-db (CloudNativePG) at examples/oversubscribed/deploy/postgres.yaml:1)
      When the pools fill, under load or with slow queries, new connections fail with SQLSTATE 53300: FATAL: remaining connection slots are reserved for roles with the SUPERUSER attribute, or FATAL: sorry, too many clients already.
      Fix: Lower the pool sizes so the total fits, for example 9 per pod across 10 pods, or lower the maximum replicas, or raise max_connections. With many pods, put PgBouncer in transaction mode in front of Postgres.

    Result: 1 finding where the pods can open more connections than Postgres accepts.
    toomanyclients reads Go source and Kubernetes manifests only. Clients in other languages, poolers such as PgBouncer, and connections from outside these files aren't counted.

`--fail-on never` is here only so the README's examples exit 0. Without it, findings exit 1.

`examples/healthy` is the same shop with an HPA maximum of 4, `DB_MAX_OPEN_CONNS` at 15 and `pool_max_conns=5` in the worker's DSN:

    $ go run ./plugin/scripts examples/healthy
    toomanyclients: 2 programs (api, worker) in 2 workloads, against 97 available connections (Cluster shop-db (CloudNativePG) at examples/healthy/deploy/postgres.yaml:1)

    Budget
      Deployment api runs api: 4 pods (HorizontalPodAutoscaler api max) x 15 per pod, +1 pod in a rollout
        sql.Open at examples/healthy/internal/store/store.go:24: SetMaxOpenConns(15) at examples/healthy/internal/store/store.go:28 (DB_MAX_OPEN_CONNS=15 in Deployment api)
      Deployment worker runs worker: 2 pods (replicas) x 5 per pod, +1 pod in a rollout
        pgxpool.New at examples/healthy/cmd/worker/main.go:15: pool_max_conns=5 in DATABASE_URL from ConfigMap worker-config at examples/healthy/deploy/worker.yaml:23
      Total: 70 at full scale, 90 during a rolling update
      Postgres: max_connections 100 - superuser_reserved_connections 3 = 97

    Result: at most 70 connections at full scale and 90 during a rolling update, within the 97 Postgres accepts.
    toomanyclients reads Go source and Kubernetes manifests only. Clients in other languages, poolers such as PgBouncer, and connections from outside these files aren't counted.

The same files against a server with `max_connections = 80` fit at full scale but not during a deploy:

    $ go run ./plugin/scripts --fail-on never --max-connections 80 examples/healthy
    toomanyclients: 2 programs (api, worker) in 2 workloads, against 77 available connections (--max-connections)

    Budget
      Deployment api runs api: 4 pods (HorizontalPodAutoscaler api max) x 15 per pod, +1 pod in a rollout
        sql.Open at examples/healthy/internal/store/store.go:24: SetMaxOpenConns(15) at examples/healthy/internal/store/store.go:28 (DB_MAX_OPEN_CONNS=15 in Deployment api)
      Deployment worker runs worker: 2 pods (replicas) x 5 per pod, +1 pod in a rollout
        pgxpool.New at examples/healthy/cmd/worker/main.go:15: pool_max_conns=5 in DATABASE_URL from ConfigMap worker-config at examples/healthy/deploy/worker.yaml:23
      Total: 70 at full scale, 90 during a rolling update
      Postgres: max_connections 80 - superuser_reserved_connections 3 = 77

    WARN TC102 examples/healthy/deploy/api.yaml:21  Pods fit at full scale (70 of 77), but a rolling update adds pods and can open 90
      Deployment api (examples/healthy/deploy/api.yaml:21) runs api: 4 pods (HorizontalPodAutoscaler api max) x 15 = 60; a rollout adds 1 pod
      Deployment worker (examples/healthy/deploy/worker.yaml:1) runs worker: 2 pods (replicas) x 5 = 10; a rollout adds 1 pod
      Postgres: max_connections 80 - superuser_reserved_connections 3 = 77 (from --max-connections)
      During a rollout, old and new pods run at the same time, each with a full pool. Pods that are shutting down can hold theirs a little longer.
      Fix: Lower the pool sizes so the total fits, for example 9 per pod across 8 pods, or lower the maximum replicas, or raise max_connections, or set maxSurge to 0 with a maxUnavailable above 0. With many pods, put PgBouncer in transaction mode in front of Postgres.

    Result: 1 finding where the pods can open more connections than Postgres accepts.
    toomanyclients reads Go source and Kubernetes manifests only. Clients in other languages, poolers such as PgBouncer, and connections from outside these files aren't counted.

### On real code

`tests/testdata/go-clean-template` holds five files from [evrone/go-clean-template](https://github.com/evrone/go-clean-template) (MIT). Its pool size goes from the `PG_POOL_MAX` env var through a `caarlos0/env` struct tag, a config struct, a functional option and a conversion helper to `poolConfig.MaxConns`. The project ships docker-compose files and no Kubernetes manifests, so `deploy/app.yaml` there was written for this test, with the `PG_POOL_MAX: "2"` its docker-compose file uses:

    $ go run ./plugin/scripts --max-connections 100 tests/testdata/go-clean-template
    toomanyclients: 1 program (app) in 1 workload, against 97 available connections (--max-connections)

    Budget
      Deployment app runs app: 3 pods (replicas) x 2 per pod, +1 pod in a rollout
        pgxpool.NewWithConfig at tests/testdata/go-clean-template/pkg/postgres/postgres.go:56: MaxConns 2 at tests/testdata/go-clean-template/pkg/postgres/postgres.go:52 (PG_POOL_MAX=2 in Deployment app)
      Total: 6 at full scale, 8 during a rolling update
      Postgres: max_connections 100 - superuser_reserved_connections 3 = 97

    Result: at most 6 connections at full scale and 8 during a rolling update, within the 97 Postgres accepts.
    toomanyclients reads Go source and Kubernetes manifests only. Clients in other languages, poolers such as PgBouncer, and connections from outside these files aren't counted.

### What Postgres really does

`examples/repro` opens one pool per simulated pod and fills every pool at once with `pg_sleep` queries. These lines were recorded on 2026-11-24 with PostgreSQL 17.6, `max_connections = 20`, a login role without SUPERUSER, and pgx v5.7.5:

| Pools | Result |
|---|---|
| database/sql, 3 pods x `SetMaxOpenConns(5)` = 15 | 15 queries succeeded |
| database/sql, 4 pods x 5 = 20 | 17 succeeded, 3 failed: `FATAL: remaining connection slots are reserved for roles with the SUPERUSER attribute (SQLSTATE 53300)` |
| database/sql, 2 pods with no `SetMaxOpenConns`, 60 queries | 17 succeeded, the rest failed with the same error and with `FATAL: sorry, too many clients already (SQLSTATE 53300)` |
| pgxpool, 4 pods x `MaxConns = 5` | 17 succeeded, 3 failed with the reserved-slots error |
| pgxpool, default size | `MaxConns` was 10 on a machine with 10 CPUs and a cgroup CPU limit of 4 (`cpu.max` 400000 100000). Under `taskset -c 0-1` it was 4, and under `taskset -c 0-5` it was 6 |

So the application role got exactly `max_connections - superuser_reserved_connections` = 17 connections, which is the number `toomanyclients` uses, and pgxpool's default follows the CPUs the process may run on, not the CPU limit. `make repro` runs these with your own `DATABASE_URL`; it downloads pgx, so it needs network access and Go 1.23 or newer.

## Usage

    toomanyclients [--pg-config FILE | --max-connections N] [--superuser-reserved N]
                   [--reserve N] [--node-cpus N] [--nodes N]
                   [--workload PROGRAM=WORKLOAD[/CONTAINER]]...
                   [--format text|json] [--fail-on warn|error|never]
                   PATH...

- `PATH` is a Go file, a manifest (`.yaml`, `.yml`) or a folder, read recursively. Pass the folders of the programs (their `main` packages and the packages they import) and the rendered manifests that deploy them. `vendor`, `testdata`, `node_modules`, hidden folders and test files are skipped, and so are folders with `Chart.yaml` or a kustomization: render those with `helm template` or `kustomize build` and pass the output.
- The limit comes from `--max-connections`, `--pg-config` (a `postgresql.conf`), or a CloudNativePG `Cluster` or Zalando `postgresql` object in the manifests, in that order. With none, PostgreSQL's default of 100 is assumed and reported as TC905. `superuser_reserved_connections` (default 3) and `reserved_connections` (default 0) are subtracted; `--superuser-reserved` overrides the first.
- `--reserve N` keeps N connections for clients outside these files: migrations, admin sessions, monitoring, services in other languages.
- `--node-cpus N` is the largest node's CPU count, for pgxpool pools on the default size. `--nodes N` is the node count, for DaemonSets.
- A workload runs a program when a container's name, image name (last path element, without tag) or `command[0]` is the program's name, or the workload has that name and one container. The program's name is its folder's name, as `go build` names the binary. `--workload api=api-server` overrides this.
- `--format json` prints the budget and findings for scripts and agents.

| Exit code | Meaning |
|---|---|
| 0 | No finding at the `--fail-on` level, or `--fail-on never` |
| 1 | Findings at the `--fail-on` level: `warn` by default |
| 2 | Bad options |
| 3 | The input can't be trusted, or there is nothing to compare. No result is printed, whatever `--fail-on` says |

Exit 3 covers missing paths, files of other types, empty files, binary or random data, Go that doesn't parse, Go files and manifests whose last line has no newline (how a file cut off partway usually ends), files that start like the output of a failed `go build`, `go test`, panic, `helm`, `kubectl` or `psql`, YAML that is plain text, unrendered Helm templates, workloads with no containers, autoscalers with no maximum, a `postgresql.conf` without `max_connections`, several Postgres servers with no flag to pick one, and input with no program, no pool or no workload. `go run` turns any failing exit code into 1, so build the binary when you need the exact code:

    ./toomanyclients examples/broken examples/healthy/cmd examples/healthy/internal
    toomanyclients: can't trust this input, so there is no result.
    examples/broken/rendered.yaml: starts like the output of a failed command, not YAML: "Error: INSTALLATION FAILED: template: shop/templates/api.yaml:31:20: executing \"...". If it should hold manifests, run the command that writes it again. If it isn't part of the deployment, pass only the folders with the code and the rendered manifests

## Findings

| Code | Level | Meaning |
|---|---|---|
| TC101 | error | At full scale (autoscaler maximum, or replicas) the pools add up to more than Postgres accepts |
| TC102 | warn | It fits at full scale, but not during a rolling update, when maxSurge pods run next to the old ones |
| TC103 | error | A database/sql pool has no `SetMaxOpenConns`, or 0, so each pod has no limit |
| TC104 | warn | A pgxpool pool uses the default size, the node's CPU count when above 4. Says at which CPU count the budget breaks. Pass `--node-cpus` to count it exactly |
| TC901 | warn | A pool size can't be read (a Secret, a config file, a computed value), so the totals are lower bounds |
| TC902 | warn | A program opens a pool, but no workload in the files runs it |
| TC903 | warn | A workload sets a database variable (such as `DATABASE_URL` or `DB_*`) but runs no program with a pool here, so its connections aren't counted. A note when `--reserve` is set |
| TC904 | warn | A DaemonSet's pod count needs `--nodes` |
| TC905 | warn | The Postgres limit was assumed |
| TC906 | warn | An HPA or ScaledObject scales a workload that isn't in the files |

Where sizes come from:

- **database/sql** (`sql.Open`, `sql.OpenDB`, sqlx, `gorm.Open`, pgx `stdlib.OpenDB`): the `SetMaxOpenConns` call in the same function, else the same package, else the program. 0 or less means no limit, as in database/sql's source.
- **pgxpool** (v4 and v5): `config.MaxConns`, else `pool_max_conns` in the DSN (a constant, `fmt.Sprintf`, or the env var it comes from, read from the container), else the default: the larger of 4 and `runtime.NumCPU()`, from pgx's `pool.go`.
- **pgx.Connect**: one connection.
- Values are followed through local variables, constants in any package of the input, function parameters at every call, small helpers, `os.Getenv` with `strconv.Atoi`, helpers like `envInt("X", 10)`, `flag` and `pflag` definitions with the container's args, and env struct tags of `caarlos0/env`, `sethvargo/go-envconfig`, `kelseyhightower/envconfig` and `cleanenv`, with their defaults and `required`. When a setting has several possible values, the highest is used.
- A pool inside a function called from N places in the program counts N times.

## How it compares

PostgreSQL's docs and many blog posts explain the arithmetic (pool size times pods must stay under `max_connections`), and Postgres logs the error when it happens. Linters such as `go vet`, staticcheck and golangci-lint look at one program, and Kubernetes linters such as kube-linter and kube-score look at manifests. GitHub and web searches on 2026-11-24 found no tool that reads both and adds up pool sizes across replicas. `toomanyclients` is a static check on files, so it runs in pull request CI.

## Limits

- **Go and Postgres only.** Clients in other languages aren't counted (TC903 points at workloads that look like clients). database/sql pools with a `mysql`, `sqlite` or other driver name are skipped.
- **One database.** Every pool is compared with one limit. If programs talk to different servers, pass only those of one server at a time.
- **Poolers.** With PgBouncer or pgpool in between, pass the pooler's client limit with `--max-connections`; its own pools to Postgres aren't read yet.
- **Values from config files, Secrets, viper and computed expressions** aren't read. They are reported as TC901 instead of being guessed. miniflux and google/exposure-notifications-server, which build their pool size from their own config code, both get TC901.
- **Name matching** links programs to containers by name. A binary renamed in the Dockerfile needs `--workload`.
- **Static call counting.** A pool opened in a loop, or in a function called from another helper several times, is counted once per call site. A function nothing in the input calls is counted once.
- **Pods that are shutting down** can hold connections a little longer than the rollout peak counts.
- **A file cut between two complete objects or declarations** still parses, so it can't be detected. No cut of `examples/oversubscribed`, after any line or at every 37th byte, prints a clean result, because a missing workload, env var or server shows up as a warning. A cut elsewhere in a real file can.

## FAQ

**What does "FATAL: sorry, too many clients already" mean?** Postgres has `max_connections` connections open and refuses the next one. For roles without SUPERUSER the limit is lower: `max_connections` minus `superuser_reserved_connections` (3) and `reserved_connections`, and the error is "remaining connection slots are reserved for roles with the SUPERUSER attribute". Both are SQLSTATE 53300.

**How many connections does a Go service open?** With database/sql, up to `SetMaxOpenConns` per pool, and without limit if it isn't set. With pgxpool, up to `MaxConns`, which defaults to the larger of 4 and the number of CPUs the process may run on. Multiply by the pods: at the HPA maximum, plus maxSurge during a deploy.

**What should my pool size be?** Available connections (`max_connections` minus reserved slots and other clients) divided by the most pods of every service that connects, rollout included. `toomanyclients` prints the totals so you can check a choice.

**Does a CPU limit change pgxpool's default?** No. `runtime.NumCPU` counts the CPUs the process may run on. The table above shows 10 in a container limited to 4 CPUs.

**Does it need to connect to my database or cluster?** No. It reads files with the standard library and never builds, runs or connects to anything.

**Is it an MCP server?** No. It's a skill with a Go program and a script that builds it, so Go is all it needs.

## Tested on

- Unit tests for each source of a pool size (constants across packages, env vars set, defaulted, from ConfigMaps, Secrets and `envFrom`, flags in args and `sh -c`, four struct tag libraries, `required`, parameters, helpers, fallback assignments, DSNs), each pod count (replicas, HPA, KEDA's default of 100, StatefulSet, DaemonSet, surge rounding, Recreate), the exact boundary (97 fits, 98 doesn't), limit sources, matching and `--workload`, and every refused input with and without `--fail-on never`. Integration tests build the binary and run it on the examples, on go-clean-template with and without `PG_POOL_MAX`, on each file of the oversubscribed example cut after every line and every 37th byte (no cut may exit 0 or print a clean result), and on empty, random, build output, test output, panic, helm, kubectl and psql files in place of the Go source, then a manifest, then the Postgres server object, then a `--pg-config` file, with the rest real. Each must exit 3 with nothing on stdout. Tests pass on Go 1.22.12 and Go 1.27.4, with `-race`.
- `examples/repro` was run against PostgreSQL 17.6 with the settings in the table above.
- Real code, on 2026-11-24: go-clean-template gives the exact budget shown above. miniflux (`DATABASE_MAX_CONNS` read by its own config parser) and google/exposure-notifications-server (pool size in a DSN built by a helper) get TC901 and no clean result, which is right because the tool can't see those values. All three were run with Deployments written for the test, since none ships Kubernetes manifests.
- Not tested: a real Kubernetes cluster, PgBouncer, CloudNativePG or Zalando clusters beyond reading their objects, and a fresh Claude session using the skill.

## Contributing

Contributions are welcome, from people and from people working with AI agents. See [CONTRIBUTING.md](CONTRIBUTING.md). Good first issues are [listed here](https://github.com/GillMacedo/toomanyclients/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22).

### How pull requests work

1. **Open a PR.** Fork the repo, work on a branch, and keep it to one change. Write a title that says what changes for users, because it becomes the changelog line: `feat:` goes under Added and `fix:` under Fixed.
2. **Automatic check.** Code changes need tests, and `make test` must pass. Changes over 30 lines need a linked open issue (`Closes #<number>`), and a PR can have at most 800 changed lines. If something is missing, a comment tells you what to fix.
3. **Review.** Claude reviews each PR automatically. Code PRs also get a security review and are tested in a sandbox. The review then merges the PR or asks for changes, and a small problem may become a follow-up issue instead. A PR with no new commit 14 days after a change request is closed, and you can reopen it. PRs that change CI, agent settings, npm or Python dependencies, vendored code or build files can't be merged this way, and a comment says so. The same goes for anything the security review doesn't clear.
4. **Release.** A merge that changes what users install becomes a new version within about two hours, with a GitHub Release and, if the project has one, its npm package. Docs and CI changes wait for the next release.

## AI disclosure

This project was largely generated with Claude Code and reviewed by its maintainer.

## About me

I'm Giulliano Macedo, and I build developer tools at GoTech Solutions. Follow along or get in touch:

- GitHub: [GillMacedo](https://github.com/GillMacedo)
- LinkedIn: [giulliano](https://www.linkedin.com/in/giulliano/)
- X: [@giumacedo](https://x.com/giumacedo)

If `toomanyclients` saved you some prompts, a star helps others find it.

---

Built by [Giulliano Macedo](https://github.com/GillMacedo), GoTech Solutions · [LinkedIn](https://www.linkedin.com/in/giulliano/) · [X](https://x.com/giumacedo)
