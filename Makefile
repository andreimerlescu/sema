# sema — Makefile
# ============================================================
# Usage:
#   make test          Run unit tests (alias for test-unit)
#   make test-unit     Run unit tests with race detector
#   make test-bench    Run benchmarks
#   make test-fuzz     Run all fuzz targets (30s each by default)
#   make test-all      Run unit + bench + fuzz
# ============================================================

PACKAGE      := ./...
FUZZTIME     ?= 30s
BENCHTARGET  ?= .
GO           ?= go

# Fuzz targets discovered from the test file
FUZZ_TARGETS := FuzzNew \
                FuzzAcquireN \
                FuzzReleaseN \
                FuzzSetCap \
                FuzzTryAcquireN \
                FuzzUtilization \
                FuzzConcurrentAcquireRelease \
                FuzzConcurrentMixedOps

.PHONY: test test-all test-unit test-bench test-fuzz

# Default: unit tests
test: test-unit

# Everything
test-all: test-unit test-bench test-fuzz

# Unit tests with race detector
test-unit:
	$(GO) test -race -count=1 -v $(PACKAGE)

# Benchmarks with memory stats
test-bench:
	$(GO) test -bench=$(BENCHTARGET) -benchmem -run='^$$' $(PACKAGE)

# All fuzz targets, each for FUZZTIME duration
test-fuzz:
	@for target in $(FUZZ_TARGETS); do \
		echo ""; \
		echo "=== FUZZ $$target ($(FUZZTIME)) ==="; \
		$(GO) test -fuzz=$$target -fuzztime=$(FUZZTIME) -run='^$$' $(PACKAGE) || exit 1; \
	done
