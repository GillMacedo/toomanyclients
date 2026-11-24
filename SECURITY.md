# Security

Report vulnerabilities through [GitHub's private vulnerability reporting](https://github.com/GillMacedo/toomanyclients/security/advisories/new), not in a public issue.

A case where `toomanyclients` counts connections wrong is a normal bug, unless it could lead someone to ship pool sizes that exhaust a production database. If it could, report it privately. `toomanyclients` is a diagnostic aid. It reads files, never connects to a database or cluster, and doesn't change or enforce anything.
