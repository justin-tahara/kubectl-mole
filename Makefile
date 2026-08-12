BINARY := kubectl-mole
KIND_CLUSTER := mole-dev
# Repo-local kubeconfig: kind and the e2e tests never touch ~/.kube/config.
KIND_KUBECONFIG := $(CURDIR)/.kube/config

BENCH_CLUSTER := mole-bench
BENCH_KUBECONFIG := $(CURDIR)/.kube/bench-config
KSTATUS_VERSION := v0.7.24

.PHONY: build test vet lint clean kind-up kind-down e2e \
	bench bench-run bench-up bench-down bench-check demo snapshot

build:
	go build -o bin/$(BINARY) ./cmd/kubectl-mole

test:
	go test ./...

vet:
	go vet ./...

lint:
	golangci-lint run

clean:
	rm -rf bin dist

kind-up:
	mkdir -p $(dir $(KIND_KUBECONFIG))
	kind create cluster --name $(KIND_CLUSTER) --kubeconfig $(KIND_KUBECONFIG) --wait 90s

kind-down:
	kind delete cluster --name $(KIND_CLUSTER) --kubeconfig $(KIND_KUBECONFIG)

e2e:
	KUBECONFIG=$(KIND_KUBECONFIG) MOLE_E2E_CONTEXT=kind-$(KIND_CLUSTER) \
		go test -tags e2e -timeout 20m ./...

bench-up:
	mkdir -p $(dir $(BENCH_KUBECONFIG))
	kind create cluster --name $(BENCH_CLUSTER) --config bench/kind.yaml \
		--kubeconfig $(BENCH_KUBECONFIG) --wait 120s

bench-down:
	kind delete cluster --name $(BENCH_CLUSTER) --kubeconfig $(BENCH_KUBECONFIG)

# The kubectl-status main package lives at ./cmd, so go install drops a
# binary named "cmd"; rename it. GOTOOLCHAIN=auto: this third-party tool may
# need a newer toolchain than our pinned one (setup-go v7 sets local).
bin/kubectl-status:
	GOTOOLCHAIN=auto GOBIN=$(CURDIR)/bin go install github.com/bergerx/kubectl-status/cmd@$(KSTATUS_VERSION)
	mv $(CURDIR)/bin/cmd $(CURDIR)/bin/kubectl-status

# bench-run measures against an existing bench cluster; BENCH_ARGS passes
# extra harness flags (e.g. --only sig-crashloop, --full).
bench-run: build bin/kubectl-status
	KUBECONFIG=$(BENCH_KUBECONFIG) go run ./bench --context kind-$(BENCH_CLUSTER) \
		--mole bin/$(BINARY) --kubectl-status bin/kubectl-status \
		--kubectl-status-version $(KSTATUS_VERSION) --out bench $(BENCH_ARGS)

# bench is the full pinned lifecycle: fresh cluster, measure, tear down.
bench:
	$(MAKE) bench-up
	$(MAKE) bench-run
	$(MAKE) bench-down

# bench-check re-measures and fails if mole's output grew beyond the
# committed results.csv threshold (used by CI).
bench-check: build bin/kubectl-status
	KUBECONFIG=$(BENCH_KUBECONFIG) go run ./bench --context kind-$(BENCH_CLUSTER) \
		--mole bin/$(BINARY) --kubectl-status bin/kubectl-status \
		--kubectl-status-version $(KSTATUS_VERSION) --out bench --check $(BENCH_ARGS)

# snapshot exercises the release pipeline locally: all platform binaries,
# archives, checksums, and container images, unsigned and unpublished.
snapshot:
	goreleaser release --snapshot --clean --skip=sign

# demo re-records assets/demo.gif against the mole-dev cluster (needs vhs).
demo: build
	KUBECONFIG=$(KIND_KUBECONFIG) kubectl --context kind-$(KIND_CLUSTER) apply -f assets/demo-scenario.yaml
	KUBECONFIG=$(KIND_KUBECONFIG) kubectl --context kind-$(KIND_CLUSTER) -n shop \
		wait deployment/checkout --for=condition=Available=false --timeout=90s
	vhs assets/demo.tape
