# ledger-core: tests on both backends.
#
#   make test       vet and the whole suite against SQLite (seconds)
#   make test-pg    the same suite against a throwaway PostgreSQL in Podman, then remove it
#   make pg-up      start the throwaway server by hand (and leave it running)
#   make pg-down    remove it
#
# The throwaway server listens on loopback only and keeps its data in memory. It runs
# the same PostgreSQL major version as the production host.

PG_NAME  := ledgercore-pg-test
PG_PORT  := 54329
PG_IMAGE := docker.io/library/postgres:17.11
PG_DSN   := postgres://ledger:ledgertest@127.0.0.1:$(PG_PORT)/ledger_test?sslmode=disable

.DEFAULT_GOAL := test

.PHONY: test
test: ## vet and test against SQLite
	go vet ./...
	go test -count=1 ./...

.PHONY: pg-up
pg-up: ## start a throwaway PostgreSQL and wait until it accepts connections
	-podman rm -f $(PG_NAME) >/dev/null 2>&1
	podman run -d --name $(PG_NAME) -p 127.0.0.1:$(PG_PORT):5432 \
		-e POSTGRES_USER=ledger -e POSTGRES_PASSWORD=ledgertest -e POSTGRES_DB=ledger_test \
		--tmpfs /var/lib/postgresql/data:rw $(PG_IMAGE) >/dev/null
	@echo "waiting for PostgreSQL..."
	@i=0; until podman exec $(PG_NAME) pg_isready -h 127.0.0.1 -U ledger -d ledger_test >/dev/null 2>&1; do \
		i=$$((i+1)); [ $$i -gt 60 ] && { echo "PostgreSQL did not start"; exit 1; }; sleep 1; done
	@echo "PostgreSQL is up on 127.0.0.1:$(PG_PORT)"

.PHONY: pg-down
pg-down: ## remove the throwaway PostgreSQL
	-podman rm -f $(PG_NAME) >/dev/null 2>&1

.PHONY: test-pg
test-pg: pg-up ## the whole suite against PostgreSQL, then remove the server; the exit status is the tests'
	LEDGERCORE_TEST_PG='$(PG_DSN)' go test -count=1 .; status=$$?; $(MAKE) --no-print-directory pg-down; exit $$status
