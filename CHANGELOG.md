# Changelog

All notable changes are documented here, in [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. This project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-11-24

### Added
- Find Postgres pools in Go source: database/sql (`sql.Open`, `sql.OpenDB`, sqlx, gorm, pgx stdlib), pgxpool (v4 and v5) and single pgx connections, and the programs whose `main` packages use them.
- Work out each pool's size from constants, environment variables (direct, through small helpers, and from env struct tags), flags, function parameters, `pool_max_conns` in DSNs and `config.MaxConns`, and resolve variables and flags from each container's env, ConfigMaps and args.
- Count pods from replicas, HorizontalPodAutoscaler and KEDA ScaledObject maximums, Job parallelism and `--nodes` for DaemonSets, plus maxSurge pods during a rolling update.
- Read the limit from `--max-connections`, `--pg-config` (postgresql.conf) or a CloudNativePG or Zalando object, minus `superuser_reserved_connections` and `reserved_connections`.
- Findings TC101 to TC104 and TC901 to TC906, text and JSON output, and `--fail-on`.
- Exit code 3, with no result, for empty, garbage, cut-off or failed-command input, and when nothing could be compared.
- `checking-postgres-connection-budget` skill and plugin manifest.
