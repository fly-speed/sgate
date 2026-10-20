.PHONY: build test stress weaknet vet clean

SGATE_STRESS_REPORT ?= $(CURDIR)/artifacts/stress.json

build:
	mkdir -p bin
	go build -trimpath -o bin/sgate-server ./cmd/sgate-server
	go build -trimpath -o bin/sgate-client ./cmd/sgate-client

test:
	go test -race ./...

stress:
	SGATE_STRESS=1 SGATE_STRESS_REPORT="$(SGATE_STRESS_REPORT)" go test -run '^TestStressReconnectResources$$' -v -timeout 10m ./internal/tunnel

weaknet:
	go test -race -count=3 -run '^(TestWeakNetwork|TestHeartbeatNotStarved)' -v -timeout 3m ./internal/tunnel

vet:
	go vet ./...

clean:
	rm -rf bin
