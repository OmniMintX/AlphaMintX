# Repo-root checks. Mirrors .github/workflows/ci.yml exactly.

.PHONY: check test go-check py-check web-check contracts-check e2e-check e2e-golden

check: go-check py-check web-check contracts-check e2e-check

test: check

go-check:
	cd control-plane && go build ./... && go vet ./... && test -z "$$(gofmt -l .)" && go test -race ./...

py-check:
	cd agent-plane && uv sync && uv run ruff check . && uv run mypy src/ && uv run pytest -q

web-check:
	cd web && pnpm install --no-frozen-lockfile && pnpm typecheck && pnpm test && pnpm build

contracts-check:
	uv run --with jsonschema python scripts/validate_contracts.py

# Deterministic E2E paper loop: emit twice + diff (byte-identical), replay
# twice + diff (byte-identical), diff against the committed goldens (pins
# fills, positions, reason codes, and cross-machine bytes), then a fast smoke
# on the decision and reason-code sequences.
e2e-check:
	mkdir -p out
	cd agent-plane && uv run python -m alphamintx_agent_plane.e2e.emit --runspec ../e2e/runspec.json --out ../out/proposals.jsonl
	cd agent-plane && uv run python -m alphamintx_agent_plane.e2e.emit --runspec ../e2e/runspec.json --out ../out/proposals.2.jsonl
	diff out/proposals.jsonl out/proposals.2.jsonl
	cd control-plane && go run ./cmd/paperloop -runspec ../e2e/runspec.json -proposals ../out/proposals.jsonl -out ../out/records.jsonl
	cd control-plane && go run ./cmd/paperloop -runspec ../e2e/runspec.json -proposals ../out/proposals.jsonl -out ../out/records.2.jsonl
	diff out/records.jsonl out/records.2.jsonl
	diff out/proposals.jsonl e2e/golden/proposals.jsonl
	diff out/records.jsonl e2e/golden/records.jsonl
	@seq="$$(grep -o '"decision":"[a-z]*"' out/records.jsonl | cut -d'"' -f4 | tr '\n' ' ')"; \
	test "$$seq" = "approve approve reject clip approve reject " \
		|| { echo "unexpected decision sequence: $$seq"; exit 1; }
	@codes="$$(grep -o '"code":"[A-Z0-9_]*"' out/records.jsonl | cut -d'"' -f4 | tr '\n' ' ')"; \
	test "$$codes" = "SYMBOL_NOT_WHITELISTED NOTIONAL_CAP_CLIPPED PROPOSAL_STALE " \
		|| { echo "unexpected reason-code sequence: $$codes"; exit 1; }
	grep -q '"decision":"clip","clipped_size_quote":"2000"' out/records.jsonl
	grep -q '"kind":"rejected_submission".*"reason_code":"STRATEGY_SCOPE_MISMATCH"' out/records.jsonl

# Regenerate the committed golden files from a fresh run. For INTENTIONAL
# output changes only: review the resulting diff before committing.
e2e-golden:
	mkdir -p out e2e/golden
	cd agent-plane && uv run python -m alphamintx_agent_plane.e2e.emit --runspec ../e2e/runspec.json --out ../out/proposals.jsonl
	cd control-plane && go run ./cmd/paperloop -runspec ../e2e/runspec.json -proposals ../out/proposals.jsonl -out ../out/records.jsonl
	cp out/proposals.jsonl e2e/golden/proposals.jsonl
	cp out/records.jsonl e2e/golden/records.jsonl
