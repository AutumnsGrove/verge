# The targets docs/testing.md names. The fast ones are offline; the tier that
# needs a vulnerability database is separate, because it is the one target here
# that cannot be run with the network switched off.
.PHONY: all vet vuln test race check e2e

all: check

# The static tier, minus the vulnerability scan. No network, no cloud, seconds.
# Requires staticcheck and gofumpt on PATH (go install honnef.co/go/tools/cmd/staticcheck@latest
# and mvdan.cc/gofumpt@latest).
vet:
	go vet ./...
	staticcheck ./...
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt needed:"; echo "$$out"; exit 1; \
	fi

# Split out from vet because it is not offline. govulncheck consults
# vuln.go.dev on a cold database, so this target fails without a network or a
# warm cache. docs/testing.md's "needs: nothing" applies to vet, not to this.
vuln:
	govulncheck ./...

# The unit + integration tier: everything reachable without a cloud.
# docs/testing.md budgets this at five seconds.
test:
	go test ./...

# Same tier under the race detector. The relay and the proxy both have
# concurrent readers, so this is not optional even while the tree is small.
race:
	go test -race ./...

# What a pull request must pass before review: everything that works with the
# network unplugged.
check: vet race

# The slow tiers. Both need real infrastructure and are skipped, loudly,
# when it is absent rather than failing.
e2e:
	go test -tags contract ./...
	go test -tags e2e ./e2e
