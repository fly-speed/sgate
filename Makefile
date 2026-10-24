.PHONY: build test framework-test vendor-sync stress weaknet vet clean

SGATE_STRESS_REPORT ?= $(CURDIR)/artifacts/stress.json

build:
	mkdir -p bin
	go build -trimpath -o bin/sgate-server ./cmd/sgate-server
	go build -trimpath -o bin/sgate-client ./cmd/sgate-client

framework-test:
	go test -mod=mod -race github.com/acl-dev/gin-service

vendor-sync:
	python3 scripts/sync-go-vendor.py

test: framework-test
	go test -race ./...

stress:
	SGATE_STRESS=1 SGATE_STRESS_REPORT="$(SGATE_STRESS_REPORT)" go test -run '^TestStressReconnectResources$$' -v -timeout 10m ./internal/tunnel

weaknet:
	go test -race -count=3 -run '^(TestWeakNetwork|TestNoiseWeakNetwork|TestHeartbeatNotStarved)' -v -timeout 8m ./internal/tunnel

vet:
	go vet ./...

clean:
	rm -rf bin
