# mssql-webui — run `make help` for targets.
-include .env
export

BIN := mssql-webui
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
# docker or podman; auto-detected, override with `make image CONTAINER=podman` or CONTAINER=podman in .env
CONTAINER ?= $(shell command -v docker >/dev/null 2>&1 && echo docker || echo podman)
# sa password for `make run-sqlserver`
SA_PASSWORD ?= Dev_Passw0rd
# database that `make seed` creates if needed and loads docs/*.sql into
SEED_DB ?= demo

.DEFAULT_GOAL := help

.PHONY: help dev dev-backend dev-web build test e2e image run run-sqlserver seed clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

dev: ## Run backend (:8080) and Vite dev server (:5173) together; reads .env
	@$(MAKE) -j2 dev-backend dev-web

dev-backend: ## Run the Go backend only
	cd backend && go run -ldflags "$(LDFLAGS)" .

dev-web: ## Run the Vite dev server only
	cd web && pnpm install && pnpm dev

build: ## Build the frontend and the single binary ./mssql-webui
	cd web && pnpm install && pnpm build
	cd backend && go build -ldflags "$(LDFLAGS)" -o ../$(BIN) .

test: ## go vet + go test, tsc typecheck, node tests
	cd backend && go vet ./... && go test ./...
	cd web && pnpm install && pnpm exec tsc -b && node --test 'src/**/*.test.ts'

e2e: build ## Browser tests of a data steward's day against make run-sqlserver (SQL_SERVERS from .env, else the run-sqlserver default)
	cd web && pnpm exec playwright-core install --only-shell chromium && node --test 'e2e/*.test.ts'

image: ## Build the container image mssql-webui (CONTAINER=docker|podman)
	$(CONTAINER) build --build-arg VERSION=$(VERSION) -t $(BIN) .

run: ## Run the image on :8080 with the variables from .env
	$(CONTAINER) run --rm -p 8080:8080 --env-file .env $(BIN)

run-sqlserver: ## Run a local SQL Server 2022 in the foreground on :1433 (sa / SA_PASSWORD, default Dev_Passw0rd); Ctrl-C stops it
	@echo "SQL_SERVERS='sqlserver://sa:$(SA_PASSWORD)@localhost:1433?trustservercertificate=true'"
	$(CONTAINER) run --rm --name mssql-webui-sql --platform linux/amd64 -p 1433:1433 -e ACCEPT_EULA=Y -e MSSQL_SA_PASSWORD='$(SA_PASSWORD)' mcr.microsoft.com/mssql/server:2022-latest

seed: ## Load docs/*.sql (sample tables) into SEED_DB (default demo, created if missing) of the run-sqlserver container
	$(CONTAINER) exec mssql-webui-sql /opt/mssql-tools18/bin/sqlcmd -C -b -U sa -P '$(SA_PASSWORD)' -Q "IF DB_ID(N'$(SEED_DB)') IS NULL CREATE DATABASE [$(SEED_DB)]"
	for f in docs/*.sql; do echo "== $$f"; $(CONTAINER) exec -i mssql-webui-sql /opt/mssql-tools18/bin/sqlcmd -C -I -b -U sa -P '$(SA_PASSWORD)' -d "$(SEED_DB)" < $$f || exit 1; done

clean: ## Remove the binary and built frontend assets
	rm -f $(BIN)
	find backend/dist -mindepth 1 ! -name .gitkeep -delete
