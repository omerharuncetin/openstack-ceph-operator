.PHONY: help vet test test-race build ceph-up ceph-drill ceph-down openstack-run clean

GO ?= go

help:
	@echo "Targets:"
	@echo "  make vet           - Run go vet across all Go modules"
	@echo "  make test          - Run all Go tests"
	@echo "  make test-race     - Run all Go tests with race detector (set CEPH_API_URL to hit Ceph)"
	@echo "  make build         - Build cephctl, mini-openstack, and cloud-operator into ./bin"
	@echo "  make ceph-up       - Start Ceph Squid (v19) container"
	@echo "  make ceph-drill    - Run OpenStack Glance->Cinder RBD clone drill on Ceph"
	@echo "  make ceph-down     - Stop Ceph container"
	@echo "  make openstack-run - Run mini-openstack server"
	@echo "  make clean         - Remove build artifacts and local data"

vet:
	$(GO) -C ceph/ceph-client vet ./...
	$(GO) -C openstack/mini-openstack vet ./...
	$(GO) -C openstack/cloud-operator vet ./...

test:
	$(GO) -C ceph/ceph-client test -v ./...
	$(GO) -C openstack/mini-openstack test -v ./...
	$(GO) -C openstack/cloud-operator test -v ./...

test-race:
	$(GO) -C ceph/ceph-client test -race -count=1 ./...
	$(GO) -C openstack/mini-openstack test -race -count=1 ./...
	$(GO) -C openstack/cloud-operator test -race -count=1 ./...

build:
	@mkdir -p bin
	$(GO) -C ceph/ceph-client build -o ../../bin/cephctl ./cmd/cephctl
	$(GO) -C openstack/mini-openstack build -o ../../bin/mini-openstack ./cmd/mini-openstack
	$(GO) -C openstack/cloud-operator build -o ../../bin/cloud-operator ./cmd/manager

ceph-up:
	docker compose -f ceph/docker-compose.yml up -d --build

ceph-drill:
	docker exec -it ceph-squid /drills/openstack-rbd-drill.sh

ceph-down:
	docker compose -f ceph/docker-compose.yml down

openstack-run:
	$(GO) -C openstack/mini-openstack run ./cmd/mini-openstack

clean:
	rm -rf bin .data
