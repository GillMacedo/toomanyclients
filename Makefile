.PHONY: test lint demo repro

# examples/ is its own module (repro needs pgx), and examples/broken is not
# Go on purpose, so gofmt only looks at the folders that must be formatted.
test:
	test -z "$$(gofmt -l plugin tests examples/oversubscribed examples/healthy examples/repro)"
	go vet ./...
	go test -race ./...

lint:
	claude plugin validate ./plugin --strict
	claude plugin validate . --strict

# The oversubscribed example has a finding on purpose, so exit 1 is
# expected. The healthy example must exit 0 and the broken one 3. The
# binary is built first because go run turns every failing exit code into 1.
demo:
	go build -o toomanyclients ./plugin/scripts
	./toomanyclients examples/oversubscribed || test $$? -eq 1
	./toomanyclients examples/healthy
	./toomanyclients examples/broken examples/healthy/cmd || test $$? -eq 3

# Runs real pools against a real Postgres. It downloads pgx, so it needs
# network access and Go 1.23 or newer. Set DATABASE_URL to a role without
# SUPERUSER on a server with max_connections = 20.
repro:
	cd examples && go run ./repro -pods 3 -pool 5
	cd examples && go run ./repro -pods 4 -pool 5
	cd examples && go run ./repro -kind pgxpool -pods 1 -pool 0 -queries 1
