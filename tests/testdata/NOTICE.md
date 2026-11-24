# Test data from other projects

`go-clean-template/` holds five Go files from [evrone/go-clean-template](https://github.com/evrone/go-clean-template) at commit d6d2f93 (2026-11-11), unchanged: `cmd/app/main.go`, `config/config.go`, `internal/app/app.go`, `pkg/postgres/postgres.go` and `pkg/postgres/options.go`. They are MIT licensed; the license is in `go-clean-template/LICENSE`. `go.mod` keeps only the module line, and `deploy/app.yaml` was written for these tests, because the project ships docker-compose files and no Kubernetes manifests.
