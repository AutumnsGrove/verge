# The targets docs/testing.md names. Everything here is offline and fast;
# the tiers that cost money are behind build tags and live in package dirs.
.PHONY: all vet test race check e2e

all: check

# The static tier. No network, no cloud, seconds.
vet:
	go vet ./...
	staticcheck ./...
	govulncheck ./...
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt needed:"; echo "$$out"; exit 1; \
	fi

# The unit + integration tier: everything reachable without a cloud.
# docs/testing.md budgets this at five seconds.
test:
	go test ./...

# Same tier under the race detector. The relay and the proxy both have
# concurrent readers, so this is not optional even while the tree is small.
race:
	go test -race ./...

# What a pull request must pass before review.
check: vet race

# The slow tiers. Both need real infrastructure and are skipped, loudly,
# when it is absent rather than failing.
e2e:
	go test -tags contract ./...
	go test -tags e2e ./e2e
