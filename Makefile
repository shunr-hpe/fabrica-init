# Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
#
# SPDX-License-Identifier: MIT


# Binary name
BINARY_NAME=fabrica-init
GO=go
GOFLAGS=-v

GO_FILES=$(shell find . -name "*.go" -type f)
BINARY_DIR=bin

.PHONY: help
help: ## Display this help screen
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

# Build the binary
.PHONY: build
build: $(BINARY_DIR)/$(BINARY_NAME) ## Build the binary

$(BINARY_DIR)/$(BINARY_NAME): $(GO_FILES)
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BINARY_DIR)
	$(GO) build -o $(BINARY_DIR)/$(BINARY_NAME) .

# Clean build artifacts
.PHONY: clean
clean: ## Clean local build artifacts
	@echo "Cleaning build artifacts..."
	rm -rf $(BINARY_DIR)
	rm -f coverage.out coverage.html

# Run the unit tests
.PHONY: test
test: ## Run the unit tests
	$(GO) test -cover -v ./...

.DEFAULT_GOAL := help
