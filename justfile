# Root Justfile for the Shared Key-Value Store

default:
	@just --list

install: install-python install-typescript install-elixir install-go

install-python:
	@echo "Setting up Python virtual environment and dependencies..."
	python3 -m venv clients/python/.venv
	./clients/python/.venv/bin/pip install --upgrade pip
	./clients/python/.venv/bin/pip install -e clients/python/[dev]

install-typescript:
	@echo "Installing TypeScript npm dependencies..."
	cd clients/typescript && npm install

install-elixir:
	@echo "Getting Elixir dependencies..."
	cd clients/elixir && mix deps.get

install-go:
	@echo "Installing Go reader dependencies..."
	cd services/reader && go mod tidy

test: test-python test-typescript test-elixir test-go

test-python:
	@echo "Running Python Client Integration Tests (via Testcontainers)..."
	./clients/python/.venv/bin/pytest -v clients/python/test_store.py

test-typescript:
	@echo "Running TypeScript Client Integration Tests (via Testcontainers)..."
	cd clients/typescript && npm test

test-elixir:
	@echo "Running Elixir Client Integration Tests (via Testcontainers)..."
	cd clients/elixir && mix test

test-go:
	@echo "Running Go Reader Service Integration Tests (via Testcontainers)..."
	cd services/reader && go test -v ./...
