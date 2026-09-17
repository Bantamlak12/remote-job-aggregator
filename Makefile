.PHONY: run test test-race test-integration vet fmt migrate-up migrate-down docker-up docker-down build

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
test-integration:
	TEST_DATABASE_URL=postgres://aggregator:aggregator@localhost:5432/aggregator_test go test ./...

docker-up:
	docker compose up -d db

docker-down:
	docker compose down
