# mssql-webui — run `make help` for targets.
-include .env
export

BIN := mssql-webui
# docker or podman; auto-detected, override with `make image CONTAINER=podman` or CONTAINER=podman in .env
CONTAINER ?= $(shell command -v docker >/dev/null 2>&1 && echo docker || echo podman)
# sa password for `make run-sqlserver`
SA_PASSWORD ?= Dev_Passw0rd

.DEFAULT_GOAL := help

.PHONY: help dev dev-backend dev-web build test image run run-sqlserver clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

dev: ## Run backend (:8080) and Vite dev server (:5173) together; reads .env
	@$(MAKE) -j2 dev-backend dev-web

dev-backend: ## Run the Go backend only
	cd backend && go run .

dev-web: ## Run the Vite dev server only
	cd web && pnpm install && pnpm dev

build: ## Build the frontend and the single binary ./mssql-webui
	cd web && pnpm install && pnpm build
	cd backend && go build -o ../$(BIN) .

test: ## go vet + go test, tsc typecheck
	cd backend && go vet ./... && go test ./...
	cd web && pnpm install && pnpm exec tsc -b

image: ## Build the container image mssql-webui (CONTAINER=docker|podman)
	$(CONTAINER) build -t $(BIN) .

run: ## Run the image on :8080 with the variables from .env
	$(CONTAINER) run --rm -p 8080:8080 --env-file .env $(BIN)

run-sqlserver: ## Run a local SQL Server 2022 in the foreground on :1433 (sa / SA_PASSWORD, default Dev_Passw0rd); Ctrl-C stops it
	@echo "SQL_SERVERS='sqlserver://sa:$(SA_PASSWORD)@localhost:1433?trustservercertificate=true'"
	$(CONTAINER) run --rm --platform linux/amd64 -p 1433:1433 -e ACCEPT_EULA=Y -e MSSQL_SA_PASSWORD='$(SA_PASSWORD)' mcr.microsoft.com/mssql/server:2022-latest

clean: ## Remove the binary and built frontend assets
	rm -f $(BIN)
	find backend/dist -mindepth 1 ! -name .gitkeep -delete
