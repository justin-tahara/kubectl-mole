BINARY := kubectl-mole
KIND_CLUSTER := mole-dev
# Repo-local kubeconfig: kind and the e2e tests never touch ~/.kube/config.
KIND_KUBECONFIG := $(CURDIR)/.kube/config

.PHONY: build test vet lint clean kind-up kind-down e2e

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
