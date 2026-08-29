.PHONY: build test fmt vet check

build:
	go build ./...

test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

check: fmt vet build test
