DEVICE ?= dev1

.PHONY: secrets up down logs devices shell test

secrets:
	./scripts/gen-secrets.sh

up: secrets
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f proxy target

devices:
	docker compose exec client sshtun devices

shell:
	docker compose exec -it client sshtun shell $(DEVICE)

test:
	cd target/agent && npm ci --no-audit --no-fund
	cd proxy && go test -race ./...
	cd client && go vet ./...
