# Developer tasks. CI runs the same commands; see .github/workflows/ci.yml.
# Tool versions match the workflows, where Renovate keeps them current.

GO ?= go
FUZZTIME ?= 30s
# renovate: datasource=go depName=golang.org/x/vuln
GOVULNCHECK_VERSION ?= v1.9.0
# renovate: datasource=go depName=github.com/google/go-licenses/v2
GO_LICENSES_VERSION ?= v2.0.1

SCRIPTS := packaging/install.sh $(wildcard packaging/scripts/*.sh) $(wildcard test/*.sh) $(wildcard test/*/*.sh)

.PHONY: help check lint test fuzz proto shellcheck vulncheck licenses snapshot release-check units live-tests vm-tests

help: ## List the targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

check: lint test proto shellcheck ## What CI checks first: lint, tests, protobuf and shell scripts

lint: ## golangci-lint for Linux, with the live tests, and for the Windows build
	golangci-lint run ./...
	golangci-lint run --build-tags integration ./...
	GOOS=windows golangci-lint run ./...

test: ## Unit tests with the race detector
	$(GO) test -race -count=1 ./...

fuzz: ## Every fuzz target for FUZZTIME each
	FUZZTIME=$(FUZZTIME) sh test/fuzz.sh

proto: ## buf lint and format, and the generated code matches the schema
	buf lint
	buf format -d --exit-code
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go
	buf generate
	git diff --exit-code -- gen

shellcheck: ## The install script, the package scripts and the test scripts
	shellcheck -s sh $(SCRIPTS)

vulncheck: ## Known vulnerabilities the code can reach
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

licenses: ## No dependency under a copyleft or unknown licence
	$(GO) run github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION) check ./... --disallowed_types=forbidden,restricted,unknown

snapshot: ## Every archive and package in dist/, unsigned, as a release builds them
	goreleaser release --snapshot --clean --skip=sign --parallelism 1

# cosign 3.1.4, the version install.sh pins, for release-check.
COSIGN ?= cosign

release-check: snapshot ## test/release/verify.sh on made-up releases with every fault, and on the snapshot, signed with a throwaway key
	sh test/release/run.sh $(COSIGN) dist

units: ## Rewrite packaging/systemd from the unit templates after changing them
	$(GO) test ./agent/internal/units -update

# One live test runs the test binary again as the snugbox user, so it is
# installed where that user can reach it.
LIVE_TEST ?= /usr/local/libexec/snugbox-tests/helper.test

live-tests: snapshot ## As root on a disposable machine: the package's whole life, the helper's live tests, the watcher's automatic undo, the budgets on a month of history, a script install, the network and secret checks and self-update
	$(GO) test -tags integration -c -o dist/helper.test ./agent/internal/helper
	sudo install -D -m 0755 dist/helper.test $(LIVE_TEST)
	rm -f dist/history.db dist/history.db-wal dist/history.db-shm
	SNUGBOX_SOAK=1 SNUGBOX_SOAK_DB=$(CURDIR)/dist/history.db $(GO) test -count=1 -run TestThirtyDaysOfFiftyWorkloads ./agent/internal/store
	sudo env DURING='$(LIVE_TEST) -test.v -test.count=1 -test.run TestLive && sh test/autoundo/run.sh && sh test/budget/run.sh 10 dist/history.db' sh test/package/run.sh dist/snugbox_*_amd64.deb
	sudo sh test/watch/run.sh dist/snugbox_*_linux_amd64.tar.gz
	sudo sh test/net/run.sh dist/snugbox_*_linux_amd64.tar.gz
	sudo sh test/secrets/run.sh dist/snugbox_*_linux_amd64.tar.gz
	sh test/update/prepare.sh dist/update
	sudo sh test/update/run.sh dist/update

vm-tests: ## Every supported distribution in a KVM virtual machine, with a reboot in the middle of a fix; needs /dev/kvm, qemu and cloud-image-utils
	sh test/vm/prepare.sh dist/vm
	sh test/vm/host.sh dist/vm $(DISTROS)
