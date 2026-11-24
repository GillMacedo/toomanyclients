# toomanyclients plugin

This plugin adds the `checking-postgres-connection-budget` skill. It reads Go source and rendered Kubernetes manifests, finds every Postgres pool a program opens (database/sql, sqlx, gorm, pgxpool, pgx), works out each pool's size from the code, env vars, ConfigMaps and container args, multiplies by the pods at full scale and during a rolling update, and compares the total with `max_connections`. It never connects to a database or a cluster.

It needs Go 1.22 or newer: `scripts/run.sh` builds the tool from the bundled source each time it runs. See the repository README for usage, findings and limits.

Install it from this repository's marketplace:

    claude plugin marketplace add GillMacedo/toomanyclients
    claude plugin install toomanyclients@toomanyclients-marketplace
