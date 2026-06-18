SOURCE := ./cmd/easyshift
MK_SOURCE := Makefile
TARGET := easyshift

CHECKMAKE := go run github.com/checkmake/checkmake/cmd/checkmake@v0.3.2
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/cmd/golangci-lint@v1.59.1
ACTIONLINT := go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12


.DEFAULT_GOAL := build


.PHONY: check

check: lint.light build test

.PHONY: lint.go.vet
lint.go.vet:
	@echo "vetting go code..."
	@go vet ./...


.PHONY: lint.go.fmt
lint.go.fmt:
	@echo "Checking go formatting..."
	@if [ -n "$$(gofmt -l .)" ]; then \
			echo "Files need formatting:"; \
				gofmt -l .; \
				exit 1; \
	else \
		echo "All files formatted correctly."; \
	fi


.PHONY: fix.go.fmt
fix.go.fmt: # fix go formatting (if needed)
	@ go fmt ./...

.PHONY: lint.go.light
lint.go.light: lint.go.vet lint.go.fmt




.PHONY: test
test:  lint.go.light ## run unit tests
	@go test -count=1 ./...

.PHONY: lint.go.golangci
lint.go.golangci: ## lint with golangci-lint
	@echo "linting go code ..."
	@$(GOLANGCI_LINT) run

.PHONY: lint.go.full
lint.go.full:  lint.go.light lint.go.golangci




.PHONY: lint.make

lint.make:
	@echo "linting the Makefile..."
	@$(CHECKMAKE) $(MK_SOURCE)
	@echo "the Makefile is OK."

.PHONY: lint.light

lint.light: lint.make lint.workflows lint.go.light


.PHONY: lint

lint: lint.make lint.workflows lint.go.full

.PHONY: lint.workflows

lint.workflows: ## lint the GitHub workflow files
	@echo "linting GitHub workflows..."
	@$(ACTIONLINT) -color
	@echo "workflows are good."




.PHONY: build

build: lint.go.light ## build the binary
	@go build -o $(TARGET) $(SOURCE)

.PHONY: all

all: build

.PHONY: clean

clean:
	@$(RM) $(TARGET)

