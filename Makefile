SHELL := /bin/bash
GOBIN ?= $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)
COMPOSE := docker compose -f deploy/docker-compose.yml --env-file .env.dev

.PHONY: all proto build test test-integration load lint up down logs devtoken banksim tf-validate migrate-check

all: proto build test

proto:
	buf generate

build:
	go build ./...

test:
	go test -race -count=1 ./pkg/... ./services/... ./cmd/...

test-integration:
	go test -race -count=1 -tags=integration -timeout=10m ./tests/integration/...

load:
	$(COMPOSE) run --rm k6 run /scripts/sprint1.js

lint:
	golangci-lint run ./...

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down -v

logs:
	$(COMPOSE) logs -f

devtoken:
	go run ./cmd/devtoken --accounts "$(ACCOUNTS)"

banksim:
	go run ./cmd/banksim $(ARGS)

tf-validate:
	cd infra/terraform/envs/dev && terraform init -backend=false -input=false >/dev/null && terraform validate
	cd infra/terraform/envs/prod && terraform init -backend=false -input=false >/dev/null && terraform validate
