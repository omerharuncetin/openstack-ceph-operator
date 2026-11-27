# Ceph Squid (v19) Local Cluster

## Components inside the container

- `ceph-mon`: Keeps cluster maps (`monmap`, `osdmap`, `crushmap`) and enforces CephX auth.
- `ceph-mgr`:
  - Dashboard REST API v1.0 at `https://localhost:8443/api`
  - Prometheus metrics at `http://localhost:9283/metrics`
- `ceph-osd` (`osd.0`): BlueStore OSD backed by a 6 GiB sparse loop file (`/var/lib/ceph/osd-0.img`). `osd_memory_target` is set to 1 GiB (Squid v19 minimum is ~939 MiB).
- OpenStack pools and keyrings: Creates `images`, `volumes`, and `vms` RBD pools plus scoped keyrings (`client.glance`, `client.cinder`, `client.nova`) in `/etc/ceph/keyrings/`.

## Commands

Start Ceph:

```bash
docker compose -f ceph/docker-compose.yml up -d --build
```

Check status (wait ~30s after first start):

```bash
docker exec -it ceph-squid ceph -s
```

Run the CephX and RBD copy-on-write clone drill:

```bash
docker exec -it ceph-squid /drills/openstack-rbd-drill.sh
```

Query Ceph with the Go CLI (`ceph/ceph-client`):

```bash
./bin/cephctl health
./bin/cephctl pools
./bin/cephctl rbd-clone-demo
```

Run the Go client tests against the container:

```bash
CEPH_API_URL=https://localhost:8443 go -C ceph/ceph-client test -v ./pkg/cephapi -run TestLiveCeph
```
