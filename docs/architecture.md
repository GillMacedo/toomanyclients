# Architecture

`toomanyclients` is one Go `package main` in `plugin/scripts`, with no dependencies. The plugin folder has no `go.mod`, so `run.sh` builds the files by name.

## Flow

1. **Files** (`scan.go`). Arguments are files or folders. Folders are walked for `.go`, `.yaml` and `.yml`, skipping `vendor`, `testdata`, `node_modules`, hidden folders, test files, and folders with `Chart.yaml` or a kustomization. Every file must be non-empty UTF-8 text that doesn't start like failed command output. Go files and manifests must end with a newline, because a file cut off mid-line usually doesn't. YAML files in a folder that never mention `apiVersion` or `kind` are skipped if they are valid YAML, and refused if they aren't.
2. **Go** (`eval.go`, `model.go`). Files are grouped into packages, with import paths from the nearest `go.mod` (or, without one, by matching import path suffixes to folders). A program is a `main` package plus every package of the input it imports. Pools are calls to `sql.Open`, `sql.OpenDB`, sqlx, `gorm.Open`, pgx `stdlib.OpenDB`, `pgxpool.New`, `NewWithConfig`, `Connect`, `ConnectConfig`, and pgx `Connect`. Pools for other databases (the driver name is a constant such as `mysql`) are left out.
3. **Pool size**. For database/sql, `SetMaxOpenConns` calls in the same function, then the same package, then the program. For pgxpool, `MaxConns` assignments, then `pool_max_conns` in the DSN given to `New` or `ParseConfig`, then the default. Values are evaluated as an `amount`: a constant, an environment variable or a flag (each with an optional default), or unknown with a reason. The evaluator follows local variables (all assignments, highest constant wins), package constants across packages, function parameters through every call in the input, small helpers whose last statement returns a value, helpers that call `os.Getenv` with a parameter, `strconv.Atoi`, flag definitions, and env struct tags of the field's name.
4. **Copies**. A pool opened inside a function that the program calls from N places counts N times. A function that nothing in the input calls counts once.
5. **Manifests** (`k8s.go`). Deployments, StatefulSets, DaemonSets, Jobs, CronJobs, ReplicaSets, Pods, HPAs, KEDA ScaledObjects, ConfigMaps, CloudNativePG Clusters and Zalando `postgresql` objects. Workloads must have containers with names and images, and autoscalers a target and a maximum, or the input is refused as cut off.
6. **Matching** (`check.go`). A container runs a program when its name, its image's last path element without tag, or `command[0]`'s base name equals the program name (the folder name, or the module path's last element at the module root, also without the part after a dot), or when the workload has that name and one container. `--workload` replaces this for a program.
7. **Budget**. Per container: each pool's size resolved against that container's env, `envFrom`, ConfigMaps and args. Per workload: pods are the autoscaler maximum, else replicas (1 when unset), Job parallelism, or `--nodes` for DaemonSets. A Deployment with RollingUpdate adds maxSurge pods (25% by default, rounded up). The limit is `max_connections - superuser_reserved_connections - reserved_connections - --reserve`.
8. **Findings and output** (`main.go`). Text or JSON. Exit 0, 1, 2 or 3.

## Rules

- Anything unread is a TC9xx warning and keeps the result from being clean.
- When a setting has several possible values, the highest is used.
- pgxpool's default is `max(4, runtime.NumCPU())`. Without `--node-cpus` it is counted as 4, marked as a lower bound, and TC104 says at which CPU count the budget breaks.
