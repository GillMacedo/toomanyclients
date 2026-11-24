---
name: checking-postgres-connection-budget
description: Checks whether Go services can open more Postgres connections than the server accepts once they run in Kubernetes. Adds up database/sql SetMaxOpenConns, pgxpool MaxConns and pool_max_conns across replicas, HPA and KEDA maximums and rolling-update surge pods, and compares the total with max_connections minus reserved slots. Use when pods log "sorry, too many clients already" or "remaining connection slots are reserved", when someone raises replicas, an HPA maximum or a pool size, or before scaling a Go service that talks to Postgres.
---

# Checking the Postgres connection budget

Each pod has its own pool. The database sees the sum: pool size times pods, for every program that connects. An HPA can raise the pod count to its maximum, and a rolling update runs extra pods next to the old ones, each with a full pool. Postgres keeps `superuser_reserved_connections` (3 by default) for superusers, so an application role gets `max_connections - 3`.

## Run the tool

It needs Go 1.22 or newer. It only reads files and never contacts a database or a cluster.

    sh "${CLAUDE_PLUGIN_ROOT}/scripts/run.sh" [--pg-config postgresql.conf | --max-connections N] CODE_AND_MANIFEST_PATHS...

- Pass the folders with the Go programs (their `main` packages and the packages they import) and the rendered manifests that deploy them. Folders are read recursively.
- For a Helm chart, render it first with the values the user deploys: `helm template RELEASE ./chart -f values.yaml > /tmp/rendered.yaml`, then pass that file. For kustomize, run `kustomize build DIR > /tmp/rendered.yaml`. Chart and kustomization folders are skipped in folder walks.
- The Postgres limit comes from, in order: `--max-connections`, `--pg-config`, or a CloudNativePG `Cluster` or Zalando `postgresql` object in the manifests. With none, PostgreSQL's default of 100 is assumed and reported as TC905. On a managed database, ask the user for `SHOW max_connections;` rather than connecting yourself.
- `--reserve N` keeps connections for clients outside these files, such as migrations, admin tools and other services.
- `--node-cpus N` gives the largest node's CPU count, for pgxpool pools left on the default size. `--nodes N` gives the node count for DaemonSets.
- When a program isn't matched to its workload (TC902), pass `--workload PROGRAM=WORKLOAD`.
- Add `--format json` to read the result in detail.

Exit codes: 0 means the budget fits, 1 means findings, 2 means bad options, and 3 means the input can't be trusted or nothing could be compared. With exit 3 there is no result, so never report the budget as fine.

## Read the result

- `TC101` (error): at full scale (HPA or KEDA maximum, or replicas) the pools add up to more than Postgres accepts.
- `TC102` (warn): it fits at full scale, but not during a rolling update, when surge pods run next to the old ones.
- `TC103` (error): a database/sql pool has no `SetMaxOpenConns` (or 0), so each pod has no limit at all.
- `TC104` (warn): a pgxpool pool uses the default size, which is the node's CPU count when above 4. The message says at which node size the budget breaks.
- `TC901`: a pool size comes from somewhere the tool can't read (a Secret, a config file, a computed value). The totals are lower bounds.
- `TC902`: a program opens a pool but no workload in the files runs it. `TC903`: a workload looks like a Postgres client but its connections weren't counted. `TC904`: a DaemonSet with no `--nodes`. `TC905`: the limit was assumed. `TC906`: an HPA or ScaledObject targets a workload that isn't in the files.

## Report

1. Give the totals from the Budget section: per workload, pods times pool size, and the limit.
2. For each finding, name the pool (`file:line`), where its size came from (constant, env var, ConfigMap, flag) and the workload.
3. Propose the smallest fix: lower the pool size, cap the HPA maximum, set `maxSurge: 0` where a rollout is the only problem, or raise `max_connections` if the server has the memory for it. With many pods, suggest PgBouncer in transaction mode.
4. Say what wasn't counted: TC9xx warnings, clients in other languages, connections from outside the files.

## Don't

Don't edit files unless the user asks. Don't connect to the database or cluster without asking. Don't call the budget safe when the tool exited 3 or reported any TC9xx warning.
