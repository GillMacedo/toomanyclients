# Roadmap

## Next
- PgBouncer: read `pgbouncer.ini` and check pools against `default_pool_size` and `max_client_conn`
- Per-role and per-database limits (`ALTER ROLE ... CONNECTION LIMIT`, `rolconnlimit`)
- MySQL `max_connections` for database/sql pools with the mysql driver, which are skipped today
- Argo Rollouts `Rollout` objects and their canary surge

## Later
- Pool sizes from viper defaults and common config file formats
- A GitHub Action that renders a chart with several values files and runs toomanyclients on each
- Comparing two renders, to report a budget change before it ships
