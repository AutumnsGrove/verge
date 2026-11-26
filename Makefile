# The targets docs/testing.md names. The fast ones are offline; the tier that
# needs a vulnerability database is separate, because it is the one target here
# that cannot be run with the network switched off.
.PHONY: all vet vuln test race check contract e2e

all: check

# The static tier, minus the vulnerability scan. No network, no cloud, seconds.
# Requires staticcheck on PATH (go install honnef.co/go/tools/cmd/staticcheck@latest).
# gofmt is the stdlib one; gofumpt is not used by this target.
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

# The tier that needs real infrastructure: a Hetzner project, an R2 bucket and a
# tailnet key. Every test in it skips with a stated reason when its credential is
# absent, so this is safe to run without any of them -- and a fork's red X stays a
# skip.
contract:
	go test -tags contract ./...

# The full tier: the contract tests, plus the directory that will hold a real
# session. That directory is not written yet, so the second line fails rather than
# pretending: a target that quietly did nothing would be worse.
e2e: contract
	go test -tags e2e ./e2e
