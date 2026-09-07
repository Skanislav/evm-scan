GO      ?= go
BIN     := bin
SOLC    ?= 0.8.28
PKGS    := ./...

.PHONY: all build test test-evm vet fmt lint contracts clean devchain demo run tidy check

all: build

## build: compile every binary into ./bin
build:
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/evmscand       ./cmd/evmscand
	$(GO) build -o $(BIN)/evmscan-demo   ./cmd/evmscan-demo
	$(GO) build -o $(BIN)/evmscan-verify ./cmd/evmscan-verify
	@echo "built: $(BIN)/evmscand $(BIN)/evmscan-demo $(BIN)/evmscan-verify"

## test: run unit tests (integration tests skip unless EVMSCAN_TEST_NODE is set)
test:
	$(GO) test $(PKGS)

vet:
	$(GO) vet $(PKGS)

fmt:
	gofmt -l -w $(shell find . -name '*.go' -not -path './vendor/*')

tidy:
	$(GO) mod tidy

## test-evm: run the lens against a real EVM (own module: heavy, test-only deps)
test-evm:
	cd contracts/evmtest && $(GO) test ./...

## check: what CI runs
check: fmt vet test test-evm

## contracts: recompile Solidity into contracts/out (requires node)
contracts:
	@command -v node >/dev/null || { echo "node is required to compile contracts"; exit 1; }
	@test -d scripts/node_modules || npm --prefix scripts install solc@$(SOLC) --no-audit --no-fund
	SOLC_HOME=$(PWD)/scripts node scripts/compile.js

## devchain: run a local geth dev node (foreground)
devchain:
	@command -v geth >/dev/null || { echo "geth not on PATH; see docs/DEMO.md to build it"; exit 1; }
	geth --dev --dev.period 2 --datadir .devchain \
		--http --http.addr 127.0.0.1 --http.port 8545 --http.api eth,net,web3 \
		--ipcpath $(PWD)/.devchain/geth.ipc

## demo: deploy contracts, generate traffic and write config.demo.yaml
demo: build
	$(BIN)/evmscan-demo -node $(PWD)/.devchain/geth.ipc -out config.demo.yaml

## run: start the indexer and API against config.demo.yaml
run: build
	$(BIN)/evmscand -config config.demo.yaml

clean:
	rm -rf $(BIN)
