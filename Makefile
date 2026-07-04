# Repo-root checks. Mirrors .github/workflows/ci.yml exactly.

.PHONY: check test go-check py-check web-check contracts-check

check: go-check py-check web-check contracts-check

test: check

go-check:
	cd control-plane && go build ./... && go vet ./... && test -z "$$(gofmt -l .)" && go test -race ./...

py-check:
	cd agent-plane && uv sync && uv run ruff check . && uv run mypy src/ && uv run pytest -q

web-check:
	cd web && pnpm install --no-frozen-lockfile && pnpm typecheck && pnpm test && pnpm build

contracts-check:
	uv run --with jsonschema python scripts/validate_contracts.py
