#!/usr/bin/env bash
# Create Ceph RBD pools and scoped CephX keyrings for Glance, Cinder, and Nova.
set -euo pipefail

PG_NUM_IMAGES="${PG_NUM_IMAGES:-64}"
PG_NUM_VOLUMES="${PG_NUM_VOLUMES:-128}"
PG_NUM_VMS="${PG_NUM_VMS:-128}"
OUTPUT_DIR="${OUTPUT_DIR:-./keyrings}"

mkdir -p "${OUTPUT_DIR}"

echo "==> Creating isolated RBD pools for Glance, Cinder, and Nova"
for spec in "images:${PG_NUM_IMAGES}" "volumes:${PG_NUM_VOLUMES}" "vms:${PG_NUM_VMS}"; do
  pool="${spec%%:*}"
  pgs="${spec##*:}"
  ceph osd pool create "${pool}" "${pgs}" "${pgs}"
  ceph osd pool application enable "${pool}" rbd
  rbd pool init "${pool}"
done

echo "==> Generating least-privilege CephX keyrings"
# Glance: write-only to images pool
ceph auth get-or-create client.glance \
  mon 'profile rbd' \
  osd 'profile rbd pool=images' \
  -o "${OUTPUT_DIR}/ceph.client.glance.keyring"

# Cinder: rwx on volumes and vms, read-only on images (for CoW clones)
ceph auth get-or-create client.cinder \
  mon 'profile rbd' \
  osd 'profile rbd pool=volumes, profile rbd pool=vms, profile rbd-read-only pool=images' \
  -o "${OUTPUT_DIR}/ceph.client.cinder.keyring"

# Nova: rwx on vms & volumes (ephemeral + attached disks), read-only on images
ceph auth get-or-create client.nova \
  mon 'profile rbd' \
  osd 'profile rbd pool=vms, profile rbd pool=volumes, profile rbd-read-only pool=images' \
  -o "${OUTPUT_DIR}/ceph.client.nova.keyring"

chmod 0600 "${OUTPUT_DIR}"/*.keyring
echo "==> Done. Keyrings written to ${OUTPUT_DIR}/"
ceph auth ls | grep -E "client\.(glance|cinder|nova)" -A 3
