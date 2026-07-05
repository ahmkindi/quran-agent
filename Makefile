SHELL := bash
GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

.DEFAULT_GOAL := help

## ---- local go ----

.PHONY: tools
tools: ## Install buf + protoc plugins + air
	go install github.com/bufbuild/buf/cmd/buf@latest
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	go install github.com/air-verse/air@latest

.PHONY: proto
proto: ## Regenerate gRPC code from proto/
	buf generate

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

.PHONY: build
build: ## Build both services locally (agent CGO-free, web with CGO)
	CGO_ENABLED=0 go build ./services/agent/...
	CGO_ENABLED=1 go build ./services/web/...

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: test
test: ## Run tests
	go test ./...

## ---- env ----

.PHONY: env
env: ## Create .env from .env.example if missing
	@test -f .env || (cp .env.example .env && echo "created .env (fill in GOOGLE_API_KEY)")

## ---- docker: dev ----

.PHONY: dev
dev: env ## Start DEV stack (hot reload) in the foreground
	docker compose up --build

.PHONY: dev-up
dev-up: env ## Start DEV stack detached
	docker compose up --build -d

.PHONY: dev-down
dev-down: ## Stop DEV stack
	docker compose down

.PHONY: logs
logs: ## Tail DEV logs
	docker compose logs -f

## ---- livekit (A/B transport) ----

# Host LAN IP for LiveKit media candidates. MUST be a routable IP the browser
# can reach (NOT 127.0.0.1 — the browser has no loopback ICE candidate to pair).
LIVEKIT_NODE_IP ?= $(shell ip route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($$i=="src"){print $$(i+1);exit}}')

.PHONY: livekit
livekit: ## Run a self-hosted LiveKit server (dev config: mDNS+loopback ICE, host net)
	docker run --rm -it --name quran-agent-livekit --network host \
	  -v $(CURDIR)/deploy/livekit.yaml:/etc/livekit.yaml:ro \
	  livekit/livekit-server:latest --config /etc/livekit.yaml --bind 0.0.0.0 --node-ip $(LIVEKIT_NODE_IP)

## ---- observability (tracing) ----

.PHONY: obs
obs: ## Run Jaeger all-in-one for tracing (UI http://localhost:16686, OTLP :4317)
	docker run --rm -it --name quran-agent-jaeger \
	  -e COLLECTOR_OTLP_ENABLED=true \
	  -p 16686:16686 -p 4317:4317 -p 4318:4318 \
	  jaegertracing/all-in-one:latest
	@echo "then run agent+web with OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317"

## ---- docker: prod ----

.PHONY: prod
prod: env ## Start PROD stack detached
	docker compose -f docker-compose.yml -f docker-compose.prod.yml up --build -d

.PHONY: prod-down
prod-down: ## Stop PROD stack
	docker compose -f docker-compose.yml -f docker-compose.prod.yml down

## ---- misc ----

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf tmp bin dist

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
