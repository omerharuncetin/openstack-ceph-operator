#!/usr/bin/env bash
# Ceph drill for OpenStack Glance -> Cinder -> Nova RBD workflows:
#   1. Check cluster health, CRUSH tree, and pools
#   2. Verify CephX isolation (client.glance cannot access volumes)
#   3. Create a CoW RBD clone from images pool into volumes pool
#   4. Run a short RADOS write benchmark
set -euo pipefail

echo "============================================================"
echo "1. CLUSTER HEALTH, CRUSH TREE & POOLS"
echo "============================================================"
ceph -s
echo ""
ceph osd tree
echo ""
ceph osd pool ls detail

echo ""
echo "============================================================"
echo "2. CEPHX LEAST-PRIVILEGE CHECK"
echo "============================================================"
echo "-> client.glance listing images pool (should succeed):"
rbd --id glance --keyring /etc/ceph/keyrings/ceph.client.glance.keyring ls images || true

echo "-> client.glance listing volumes pool (should fail with permission denied):"
if rbd --id glance --keyring /etc/ceph/keyrings/ceph.client.glance.keyring ls volumes 2>&1; then
  echo "ERROR: glance should not have access to volumes" >&2
  exit 1
else
  echo "PASS: CephX blocked client.glance from accessing volumes pool"
fi

echo ""
echo "============================================================"
echo "3. GLANCE -> CINDER RBD COPY-ON-WRITE CLONE"
echo "============================================================"
rbd rm volumes/cinder-vol-demo 2>/dev/null || true
rbd snap unprotect images/ubuntu-24.04-base@snap1 2>/dev/null || true
rbd snap rm images/ubuntu-24.04-base@snap1 2>/dev/null || true
rbd rm images/ubuntu-24.04-base 2>/dev/null || true

echo "-> [Glance] Creating 128 MiB base image in images pool (as client.glance)"
rbd --id glance --keyring /etc/ceph/keyrings/ceph.client.glance.keyring \
  create images/ubuntu-24.04-base --size 128M --image-feature layering,exclusive-lock,object-map,fast-diff

echo "-> [Glance] Writing 8 MiB test rootfs into base image"
dd if=/dev/urandom bs=1M count=8 status=none | \
  rbd --id glance --keyring /etc/ceph/keyrings/ceph.client.glance.keyring \
  import - images/ubuntu-24.04-import --image-feature layering,exclusive-lock,object-map,fast-diff 2>/dev/null || true
rbd rm images/ubuntu-24.04-base
rbd mv images/ubuntu-24.04-import images/ubuntu-24.04-base

echo "-> [Glance] Creating and protecting snapshot @snap1"
rbd --id glance --keyring /etc/ceph/keyrings/ceph.client.glance.keyring \
  snap create images/ubuntu-24.04-base@snap1
rbd --id glance --keyring /etc/ceph/keyrings/ceph.client.glance.keyring \
  snap protect images/ubuntu-24.04-base@snap1

echo "-> [Cinder] Cloning snapshot into volumes/cinder-vol-demo (as client.cinder)"
rbd --id cinder --keyring /etc/ceph/keyrings/ceph.client.cinder.keyring \
  clone images/ubuntu-24.04-base@snap1 volumes/cinder-vol-demo

echo "-> [Verify] Clone parent-child lineage and disk usage:"
rbd info volumes/cinder-vol-demo
rbd children images/ubuntu-24.04-base@snap1
rbd du images/ubuntu-24.04-base
rbd du volumes/cinder-vol-demo

echo ""
echo "============================================================"
echo "4. RADOS WRITE BENCHMARK (5s, 4 MiB objects)"
echo "============================================================"
rados bench -p volumes 5 write -t 4 --no-cleanup
rados -p volumes cleanup --run-name benchmark_last_metadata || true

echo ""
echo "==> Drill complete"
