# db-webui — run `make help` for targets.
-include .env
export

BIN := db-webui

.DEFAULT_GOAL := help

.PHONY: help dev dev-backend dev-web build test docker clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

dev: ## Run backend (:8080) and Vite dev server (:5173) together; reads .env
	@$(MAKE) -j2 dev-backend dev-web

dev-backend: ## Run the Go backend only
	cd backend && go run .

dev-web: ## Run the Vite dev server only
	cd web && pnpm install && pnpm dev

build: ## Build the frontend and the single binary ./db-webui
	cd web && pnpm install && pnpm build
	cd backend && go build -o ../$(BIN) .

test: ## go vet + go test, tsc typecheck
	cd backend && go vet ./... && go test ./...
	cd web && pnpm install && pnpm exec tsc -b

docker: ## Build the Docker image db-webui
	docker build -t $(BIN) .

clean: ## Remove the binary and built frontend assets
	rm -f $(BIN)
	find backend/dist -mindepth 1 ! -name .gitkeep -delete
