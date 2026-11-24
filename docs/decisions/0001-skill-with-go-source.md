# 0001: A skill with Go source, not an MCP server

**Decision:** ship a Claude Code skill whose script builds a Go CLI from bundled source with the standard library only.

**Why:** the agent needs one read-only computation over files it already has. A skill with a script does that with nothing to install but Go, and the same CLI runs in CI. An MCP server would add a process, a package and a release pipeline for no extra capability.
