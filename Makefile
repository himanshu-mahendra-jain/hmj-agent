.PHONY: all verify test cover build clean check fix race fuzz bench e2e e2e-live vuln

all: verify

fix:
	go fix ./...
	go fix -newexpr ./...
	gofmt -s -w .

# Linters from golang.org/x, run by version so they stay out of go.mod and go.sum (whose
# tests would pull third-party modules into go.sum); bump by hand.
XTOOLS := v0.52.0

check:
	@test -z "$$(gofmt -s -l .)" || (echo "Unformatted files found. Run 'make fix'." && exit 1)
	@go fix -diff ./... >/dev/null && go fix -newexpr -diff ./... >/dev/null || (echo "go fix has pending rewrites. Run 'make fix'." && exit 1)
	go vet ./...
	go run golang.org/x/tools/go/analysis/passes/nilness/cmd/nilness@$(XTOOLS) ./...
	go run ./cmd/droppederr ./...
	@out=$$(go run golang.org/x/tools/cmd/deadcode@$(XTOOLS) ./...) && test -z "$$out" || (echo "$$out"; echo "Functions above are unreachable from hmj (test-only code belongs in _test.go files)."; exit 1)

# Known vulnerabilities in dependencies and the Go standard library (needs network access).
# Pinned, since it runs in the release job that builds the binaries; bump it by hand.
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

test:
	go test ./... -count=1 -shuffle=on -race -coverpkg=./... -coverprofile=cover.out
	@go tool cover -func=cover.out | tail -1
	@rm -f cover.out

# Coverage including the built binary: the unit tests write their counters to
# $(COVER_DIR)/unit, and the binary that e2e/ drives as a subprocess is built
# with -cover (HMJ_COVER_DIR) and writes to $(COVER_DIR)/bin. Both are merged into
# cover.out; `go tool cover -html=cover.out` shows it.
COVER_DIR := $(CURDIR)/.cover

cover:
	rm -rf $(COVER_DIR) && mkdir -p $(COVER_DIR)/unit $(COVER_DIR)/bin
	HMJ_COVER_DIR=$(COVER_DIR)/bin go test ./... -count=1 -covermode=atomic -coverpkg=./... -args -test.gocoverdir=$(COVER_DIR)/unit
	go tool covdata textfmt -i=$(COVER_DIR)/unit -o $(COVER_DIR)/unit.out
	go tool covdata textfmt -i=$(COVER_DIR)/unit,$(COVER_DIR)/bin -o cover.out
	@printf 'unit tests only:      '; go tool cover -func=$(COVER_DIR)/unit.out | tail -1
	@printf 'unit tests + binary:  '; go tool cover -func=cover.out | tail -1

# End-to-end: drives the built binary in a pseudo terminal against a mock provider.
# Also part of `make test` (./...); this target runs it alone, verbosely.
e2e:
	go test ./e2e -count=1 -v

# One real tool-using turn against a real provider. Needs HMJ_E2E_LIVE_CONFIG (path to a
# config) and HMJ_E2E_LIVE_MODEL (provider/model).
e2e-live:
	@test -n "$$HMJ_E2E_LIVE_CONFIG" && test -n "$$HMJ_E2E_LIVE_MODEL" || \
		(echo "Set HMJ_E2E_LIVE_CONFIG=<config path> and HMJ_E2E_LIVE_MODEL=<provider/model>." && exit 1)
	go test ./e2e -run '^TestLiveProvider$$' -count=1 -v

bench:
	go test ./... -bench=. -benchmem -run=^$$

race:
	go test ./... -race -count=5 -shuffle=on -timeout=5m

fuzz:
	go test ./internal/tools -run=^$$ -fuzz=^FuzzResolvePath$$ -fuzztime=3s
	go test ./internal/diff -run=^$$ -fuzz=^FuzzDiffText$$ -fuzztime=3s
	go test ./internal/tools -run=^$$ -fuzz=^FuzzToolArguments$$ -fuzztime=3s
	go test ./internal/chat -run=^$$ -fuzz=^FuzzParseRateLimitHint$$ -fuzztime=3s
	go test ./internal/terminal -run=^$$ -fuzz=^FuzzUnquoteDragPath$$ -fuzztime=3s
	go test ./internal/tools -run=^$$ -fuzz=^FuzzHTMLToText$$ -fuzztime=3s
	go test ./internal/tools -run=^$$ -fuzz=^FuzzFindTrimmedLineMatches$$ -fuzztime=3s
	go test ./internal/terminal -run=^$$ -fuzz=^FuzzSeamlessInputKeys$$ -fuzztime=3s
	go test ./internal/terminal -run=^$$ -fuzz=^FuzzMarkdownTableRendering$$ -fuzztime=3s

build:
	@if [ "$$ALLOW_DIRTY" != "1" ] && [ "$$HMJ_ALLOW_DIRTY" != "1" ]; then \
		if ! git diff --quiet || ! git diff --cached --quiet; then \
			echo "ERROR: Working tree is dirty; binary would be stamped +dirty."; \
			echo "       Commit changes first, or override with: ALLOW_DIRTY=1 make build"; \
			exit 1; \
		fi \
	fi
	mkdir -p $(HOME)/.local/bin
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(HOME)/.local/bin/hmj .
	cp $(HOME)/.local/bin/hmj ./hmj
	@echo "Built and installed to $(HOME)/.local/bin/hmj and ./hmj"
	@if go version -m ./hmj | grep -qE "vcs\.modified=true"; then \
		echo "VCS check: WARNING: binary is +dirty"; \
	else \
		echo "VCS check: binary is clean (matches HEAD)"; \
	fi

verify: check vuln test build

clean:
	rm -rf hmj cover.out .cover
