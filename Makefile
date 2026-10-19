.PHONY: build test vet clean

build:
	mkdir -p bin
	go build -trimpath -o bin/sgate-server ./cmd/sgate-server
	go build -trimpath -o bin/sgate-client ./cmd/sgate-client

test:
	go test -race ./...

vet:
	go vet ./...

clean:
	rm -rf bin
