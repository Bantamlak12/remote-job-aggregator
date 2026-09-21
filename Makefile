.PHONY: run test test-race test-integration test-integration-race vet fmt migrate-up migrate-down docker-up docker-down build

build:
	go build -o bin/aggregator ./cmd/aggregator

run: build
	./bin/aggregator run

migrate-up: build
	./bin/aggregator migrate-up

migrate-down: build
	./bin/aggregator migrate-down --yes

fmt:
	gofmt -w .

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

# Requires `docker compose up -d db` first. Points at the aggregator_test
# database provisioned alongside the main one — never the dev database,
# since these tests drop tables.
#
# -p 1 is not optional here: more than one package now runs migration
# resets (Up/Down) against this same database (internal/database,
# internal/company, ...), and go test's default package-level
# parallelism runs those test binaries as separate concurrent processes
# with no interlock between them — they will drop each other's tables
# mid-run. -p 1 forces one package's test binary to finish before the
# next starts; it does not affect intra-package t.Parallel, if any.
test-integration:
	TEST_DATABASE_URL=postgres://aggregator:aggregator@localhost:5432/aggregator_test go test -p 1 ./...

test-integration-race:
	TEST_DATABASE_URL=postgres://aggregator:aggregator@localhost:5432/aggregator_test go test -race -p 1 ./...

docker-up:
	docker compose up -d db

docker-down:
	docker compose down
