.PHONY: test vet check

test:
	go test ./...

vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

check: vet test
