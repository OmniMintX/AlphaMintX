# Repo-root checks. Mirrors .github/workflows/ci.yml exactly.

.PHONY: check test go-check py-check web-check contracts-check boundary-check e2e-check e2e-golden backtest-check backtest-golden

check: go-check py-check web-check contracts-check boundary-check e2e-check backtest-check

test: check

go-check:
	cd control-plane && go build ./... && go vet ./... && test -z "$$(gofmt -l .)" && go test -race ./...

py-check:
	cd agent-plane && uv sync && uv run ruff check . && uv run mypy src/ && uv run pytest -q

web-check:
	cd web && pnpm install --no-frozen-lockfile && pnpm typecheck && pnpm test && pnpm build

contracts-check:
	uv run --with jsonschema python scripts/validate_contracts.py

boundary-check:
	uv run --no-project python scripts/check_plane_boundary.py

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

# Deterministic backtest loop (docs/specs/backtest-engine.md): emit M0 twice
# + diff (byte-identical), M0-vs-M1 mask agreement (m1), dataset-slice
# snapshot recheck (m2), replay twice into fresh DBs + diff (byte-identical),
# diff against the committed goldens, then a fast smoke on decisions/fills.
BT_EMIT = uv run python -m alphamintx_agent_plane.backtest.emit \
	--dataset ../backtest/golden/dataset.jsonl --seed 42 \
	--strategy-id b7e57000-0000-4000-8000-000000000001 --scenario bullish \
	--window 24 --symbol BTC/USDT --interval 1h
BT_REPLAY = go run ./cmd/backtestctl replay -runspec ../backtest/runspec.json \
	-dataset ../backtest/golden/dataset.jsonl -proposals ../out/bt-proposals.jsonl

backtest-check:
	mkdir -p out
	rm -f out/bt-1.db out/bt-2.db
	cd agent-plane && $(BT_EMIT) --mask M0 --out ../out/bt-proposals.jsonl
	cd agent-plane && $(BT_EMIT) --mask M0 --out ../out/bt-proposals.2.jsonl
	diff out/bt-proposals.jsonl out/bt-proposals.2.jsonl
	cd agent-plane && $(BT_EMIT) --mask M1 --out ../out/bt-proposals.m1.jsonl
	cd agent-plane && uv run python -m alphamintx_agent_plane.backtest.check --mode m1 --a ../out/bt-proposals.jsonl --b ../out/bt-proposals.m1.jsonl
	cd agent-plane && uv run python -m alphamintx_agent_plane.backtest.check --mode m2 --dataset ../backtest/golden/dataset.jsonl --proposals ../out/bt-proposals.jsonl
	cd control-plane && $(BT_REPLAY) -db ../out/bt-1.db -out ../out/bt-records.jsonl
	cd control-plane && $(BT_REPLAY) -db ../out/bt-2.db -out ../out/bt-records.2.jsonl
	diff out/bt-records.jsonl out/bt-records.2.jsonl
	diff out/bt-proposals.jsonl backtest/golden/proposals.jsonl
	diff out/bt-records.jsonl backtest/golden/records.jsonl
	@n="$$(grep -c '"decision":"approve"' out/bt-records.jsonl)"; \
	test "$$n" = "22" || { echo "unexpected approve count: $$n"; exit 1; }
	@n="$$(grep -o '"code":"[A-Z0-9_]*"' out/bt-records.jsonl | grep -c MAX_POSITIONS_REACHED)"; \
	test "$$n" = "8" || { echo "unexpected MAX_POSITIONS_REACHED count: $$n"; exit 1; }
	@n="$$(grep '"type":"stop"' out/bt-records.jsonl | grep -c '"status":"filled"')"; \
	test "$$n" = "10" || { echo "unexpected stop-fill count: $$n"; exit 1; }
	@n="$$(grep '"take_profit"' out/bt-records.jsonl | grep -c '"status":"filled"')"; \
	test "$$n" = "3" || { echo "unexpected take-profit-fill count: $$n"; exit 1; }

# Regenerate the committed backtest goldens. For INTENTIONAL output changes
# only: review the resulting diff before committing.
backtest-golden:
	mkdir -p out backtest/golden
	rm -f out/bt-1.db
	cd agent-plane && $(BT_EMIT) --mask M0 --out ../out/bt-proposals.jsonl
	cd control-plane && $(BT_REPLAY) -db ../out/bt-1.db -out ../out/bt-records.jsonl
	cp out/bt-proposals.jsonl backtest/golden/proposals.jsonl
	cp out/bt-records.jsonl backtest/golden/records.jsonl
