.PHONY: build test vet lint changelog-fix

## build: Build every package
build:
	go build ./...

## test: Run the test suite
test:
	go test ./...

## vet: Run go vet
vet:
	go vet ./...

## lint: Run golangci-lint (the same target CI's Lint gate runs, #54)
#
# Keep in step with the version pinned in .github/workflows/ci.yml (Lint job).
# No .golangci.yml today, so golangci-lint's built-in defaults apply; one at the
# repo root would be picked up automatically, by CI and locally alike.
GOLANGCI_LINT_VERSION=2.14.0
lint:
	@echo "Running linter..."
	@which golangci-lint > /dev/null || (echo "golangci-lint not installed. Run: curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v$(GOLANGCI_LINT_VERSION)/install.sh | sh -s -- -b $$(go env GOPATH)/bin v$(GOLANGCI_LINT_VERSION)" && exit 1)
	@golangci-lint version 2>/dev/null | grep -q "version $(GOLANGCI_LINT_VERSION) " || echo "warning: CI pins golangci-lint $(GOLANGCI_LINT_VERSION); local $$(golangci-lint version 2>/dev/null | awk '{print $$4}') may report different findings"
	golangci-lint run

## changelog-fix: Merge duplicate group headings in CHANGELOG.md [Unreleased]
#
# Mechanical fix for the duplicate-group conflict changelog_test.go rejects.
changelog-fix:
	python3 scripts/changelog-consolidate.py CHANGELOG.md
