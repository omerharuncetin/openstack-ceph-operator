# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.work go.work.sum ./
COPY ceph/ceph-client/go.mod ceph/ceph-client/
COPY openstack/mini-openstack/go.mod openstack/mini-openstack/go.sum openstack/mini-openstack/
COPY openstack/cloud-operator/go.mod openstack/cloud-operator/go.sum openstack/cloud-operator/
RUN go -C openstack/cloud-operator mod download

COPY ceph/ceph-client ceph/ceph-client
COPY openstack/mini-openstack openstack/mini-openstack
COPY openstack/cloud-operator openstack/cloud-operator

RUN CGO_ENABLED=0 GOOS=linux go -C openstack/cloud-operator build -trimpath -ldflags="-s -w" -o /out/cloud-operator ./cmd/manager && \
    CGO_ENABLED=0 GOOS=linux go -C openstack/mini-openstack build -trimpath -ldflags="-s -w" -o /out/mini-openstack ./cmd/mini-openstack

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=builder /out/cloud-operator /cloud-operator
COPY --from=builder /out/mini-openstack /mini-openstack
USER 65532:65532
ENTRYPOINT ["/cloud-operator"]
