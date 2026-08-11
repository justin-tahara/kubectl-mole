BINARY := kubectl-mole

.PHONY: build test vet lint clean

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
