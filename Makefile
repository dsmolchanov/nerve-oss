.PHONY: up down logs seed seed-full mcp-test mcp-test-host selfhost-smoke doctor cloud-e2e-test

CONFIG ?= configs/dev/host.yaml
GOCACHE ?= /tmp/go-build
GOPATH ?= /tmp/go
GOMODCACHE ?= /tmp/go/pkg/mod
GOENV = GOCACHE=$(GOCACHE) GOPATH=$(GOPATH) GOMODCACHE=$(GOMODCACHE)

up:
	docker compose up -d --wait

down:
	docker compose down

logs:
	docker compose logs -f cortex

seed:
	docker compose exec -T cortex /app/neuralmail seed

seed-full:
	docker compose exec -T -e NERVE_SMTP_HOST=stalwart -e NERVE_SMTP_PORT=25 cortex /app/neuralmail seed

mcp-test:
	docker compose exec -T -e NERVE_HTTP_ADDR=127.0.0.1:8088 cortex /app/neuralmail mcp-test

doctor:
	NERVE_CONFIG=$(CONFIG) $(GOENV) go run ./cmd/neuralmail doctor

cloud-e2e-test:
	$(GOENV) go test ./internal/cloudapi -run TestCloudE2EMatrix -count=1

mcp-test-host:
	NERVE_CONFIG=$(CONFIG) $(GOENV) go run ./cmd/neuralmail mcp-test

selfhost-smoke:
	python3 scripts/ci/selfhost_smoke.py
