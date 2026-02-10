.PHONY: up down logs seed mcp-test doctor cloud-e2e-test

CONFIG ?= configs/dev/host.yaml
GOCACHE ?= /tmp/go-build
GOPATH ?= /tmp/go
GOMODCACHE ?= /tmp/go/pkg/mod
GOENV = GOCACHE=$(GOCACHE) GOPATH=$(GOPATH) GOMODCACHE=$(GOMODCACHE)

up:
	docker compose up -d

down:
	docker compose down

logs:
	docker compose logs -f cortex

seed:
	NM_CONFIG=$(CONFIG) $(GOENV) go run ./cmd/neuralmail seed

mcp-test:
	NM_CONFIG=$(CONFIG) $(GOENV) go run ./cmd/neuralmail mcp-test

doctor:
	NM_CONFIG=$(CONFIG) $(GOENV) go run ./cmd/neuralmail doctor

cloud-e2e-test:
	$(GOENV) go test ./internal/cloudapi -run TestCloudE2EMatrix -count=1
