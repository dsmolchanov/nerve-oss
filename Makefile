.PHONY: up down logs seed mcp-test doctor

up:
	docker compose up -d

down:
	docker compose down

logs:
	docker compose logs -f cortex

seed:
	go run ./cmd/neuralmail seed

mcp-test:
	go run ./cmd/neuralmail mcp-test

doctor:
	go run ./cmd/neuralmail doctor
