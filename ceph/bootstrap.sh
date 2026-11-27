#!/usr/bin/env bash
# Bootstraps Ceph Squid (v19):
#   - ceph-mon
#   - ceph-mgr (Dashboard REST API :8443, Prometheus :9283)
#   - ceph-osd (BlueStore on a sparse loop device)
#   - OpenStack RBD pools (images, volumes, vms) and CephX keyrings
set -euo pipefail

CLUSTER="ceph"
MON_ID="test-mon-id"
MGR_ID="test-mgr-id"
OSD_ID="0"
MON_IP="$(hostname -i | awk '{print $1}')"
# from docker-compose.yml
DASHBOARD_USER="${CEPH_DASHBOARD_USER}"
DASHBOARD_PASS="${CEPH_DASHBOARD_PASS}"
OSD_SIZE_GB="${OSD_SIZE_GB}"
OSD_MEMORY_TARGET="${OSD_MEMORY_TARGET}"
INIT_OPENSTACK_POOLS="${INIT_OPENSTACK_POOLS}"

CEPH_CONF="/etc/ceph/${CLUSTER}.conf"
ADMIN_KEYRING="/etc/ceph/${CLUSTER}.client.admin.keyring"
MON_KEYRING="/var/lib/ceph/tmp/${CLUSTER}.mon.keyring"
MON_DATA="/var/lib/ceph/mon/${CLUSTER}-${MON_ID}"
MGR_DATA="/var/lib/ceph/mgr/${CLUSTER}-${MGR_ID}"
OSD_DATA="/var/lib/ceph/osd/${CLUSTER}-${OSD_ID}"
OSD_IMG="/var/lib/ceph/osd-${OSD_ID}.img"

mkdir -p /etc/ceph /var/lib/ceph/tmp /var/run/ceph "${MON_DATA}" "${MGR_DATA}" "${OSD_DATA}" /etc/ceph/keyrings

if [[ ! -f "${CEPH_CONF}" ]]; then
  FSID="$(uuidgen)"
  echo "==> [1/5] Generating ${CEPH_CONF} (fsid=${FSID}, mon_ip=${MON_IP})"
  cat > "${CEPH_CONF}" <<EOF
[global]
fsid = ${FSID}
mon initial members = ${MON_ID}
mon host = [v2:${MON_IP}:3300,v1:${MON_IP}:6789]
public network = 0.0.0.0/0
auth cluster required = cephx
auth service required = cephx
auth client required = cephx
osd crush chooseleaf type = 0
osd pool default size = 1
osd pool default min size = 1
osd memory target = ${OSD_MEMORY_TARGET}
mon allow pool delete = true
mon warn on pool no redundancy = false
bluestore block size = $((OSD_SIZE_GB * 1024 * 1024 * 1024))

[mon.${MON_ID}]
host = $(hostname)
public addr = ${MON_IP}
EOF

  echo "==> [2/5] Creating CephX keyrings and bootstrapping MON"
  ceph-authtool --create-keyring "${MON_KEYRING}" --gen-key -n mon. --cap mon 'allow *'
  ceph-authtool --create-keyring "${ADMIN_KEYRING}" --gen-key -n client.admin \
    --cap mon 'allow *' --cap osd 'allow *' --cap mds 'allow *' --cap mgr 'allow *'
  ceph-authtool --create-keyring /var/lib/ceph/bootstrap-osd/${CLUSTER}.keyring --gen-key -n client.bootstrap-osd \
    --cap mon 'profile bootstrap-osd' --cap mgr 'allow r'
  ceph-authtool "${MON_KEYRING}" --import-keyring "${ADMIN_KEYRING}"
  ceph-authtool "${MON_KEYRING}" --import-keyring /var/lib/ceph/bootstrap-osd/${CLUSTER}.keyring

  monmaptool --create --addv "${MON_ID}" "[v2:${MON_IP}:3300/0,v1:${MON_IP}:6789/0]" --fsid "${FSID}" /var/lib/ceph/tmp/monmap
  chown -R ceph:ceph /var/lib/ceph /var/run/ceph /etc/ceph
  su -s /bin/bash ceph -c "ceph-mon --mkfs -i ${MON_ID} --monmap /var/lib/ceph/tmp/monmap --keyring ${MON_KEYRING}"
fi

echo "==> [3/5] Starting ceph-mon"
chown -R ceph:ceph /var/lib/ceph /var/run/ceph /etc/ceph
su -s /bin/bash ceph -c "ceph-mon --cluster ${CLUSTER} -i ${MON_ID}"

for _ in $(seq 1 30); do
  if timeout 3 ceph -s >/dev/null 2>&1; then break; fi
  sleep 1
done

ceph config set global osd_memory_target "${OSD_MEMORY_TARGET}" || true
ceph mon enable-msgr2 || true
ceph config set mon auth_allow_insecure_global_id_reclaim false || true

if [[ ! -f "${MGR_DATA}/keyring" ]]; then
  echo "==> [4/5] Bootstrapping ceph-mgr (Dashboard REST API + Prometheus)"
  ceph auth get-or-create "mgr.${MGR_ID}" \
    mon 'allow profile mgr' osd 'allow *' mds 'allow *' \
    -o "${MGR_DATA}/keyring"
  chown -R ceph:ceph "${MGR_DATA}"
fi
su -s /bin/bash ceph -c "ceph-mgr --cluster ${CLUSTER} -i ${MGR_ID}"

if [[ ! -f "${OSD_DATA}/fsid" ]]; then
  echo "==> [5/5] Provisioning BlueStore OSD.${OSD_ID} on sparse loop device (${OSD_SIZE_GB}G)"
  truncate -s "${OSD_SIZE_GB}G" "${OSD_IMG}"
  LOOP_DEV="$(losetup --find --show "${OSD_IMG}")"
  echo "    Attached sparse image to ${LOOP_DEV}"

  OSD_UUID="$(uuidgen)"
  OSD_SECRET="$(ceph-authtool --gen-print-key)"
  echo "{\"cephx_secret\": \"${OSD_SECRET}\"}" | ceph osd new "${OSD_UUID}" -i - -n client.bootstrap-osd -k "/var/lib/ceph/bootstrap-osd/${CLUSTER}.keyring" >/dev/null

  ceph-authtool --create-keyring "${OSD_DATA}/keyring" --name "osd.${OSD_ID}" --add-key "${OSD_SECRET}"
  ln -sf "${LOOP_DEV}" "${OSD_DATA}/block"
  chown -R ceph:ceph "${OSD_DATA}" "${LOOP_DEV}"
  su -s /bin/bash ceph -c "ceph-osd --cluster ${CLUSTER} -i ${OSD_ID} --mkfs --osd-uuid ${OSD_UUID}"
else
  # Re-attach loop device on container restart
  if [[ -f "${OSD_IMG}" ]] && ! losetup -j "${OSD_IMG}" | grep -q "${OSD_IMG}"; then
    LOOP_DEV="$(losetup --find --show "${OSD_IMG}")"
    ln -sf "${LOOP_DEV}" "${OSD_DATA}/block"
    chown ceph:ceph "${LOOP_DEV}"
  fi
fi

su -s /bin/bash ceph -c "ceph-osd --cluster ${CLUSTER} -i ${OSD_ID}"

# Enable Dashboard REST API (:8443) and Prometheus exporter (:9283)
sleep 3
ceph mgr module enable prometheus || true
ceph mgr module enable dashboard || true
ceph config set mgr mgr/dashboard/server_addr 0.0.0.0 || true
ceph config set mgr mgr/dashboard/server_port 8443 || true
ceph dashboard create-self-signed-cert || true

PASS_FILE="/tmp/ceph-dash-pass"
printf "%s" "${DASHBOARD_PASS}" > "${PASS_FILE}"
ceph dashboard ac-user-create "${DASHBOARD_USER}" -i "${PASS_FILE}" administrator --force-password >/dev/null 2>&1 || true
rm -f "${PASS_FILE}"

if [[ "${INIT_OPENSTACK_POOLS}" == "true" ]]; then
  echo "==> Initializing OpenStack pools (images, volumes, vms) and scoped CephX keyrings"
  for pool in images volumes vms; do
    if ! ceph osd pool ls | grep -qx "${pool}"; then
      ceph osd pool create "${pool}" 16 16
      ceph osd pool application enable "${pool}" rbd
      rbd pool init "${pool}"
    fi
  done
  ceph auth get-or-create client.glance mon 'profile rbd' osd 'profile rbd pool=images' \
    -o /etc/ceph/keyrings/ceph.client.glance.keyring >/dev/null
  ceph auth get-or-create client.cinder mon 'profile rbd' \
    osd 'profile rbd pool=volumes, profile rbd pool=vms, profile rbd-read-only pool=images' \
    -o /etc/ceph/keyrings/ceph.client.cinder.keyring >/dev/null
  ceph auth get-or-create client.nova mon 'profile rbd' \
    osd 'profile rbd pool=vms, profile rbd pool=volumes, profile rbd-read-only pool=images' \
    -o /etc/ceph/keyrings/ceph.client.nova.keyring >/dev/null
fi

echo "==> Ceph Squid (v19) is ready"
echo "    Dashboard REST API: https://localhost:8443/api (user=${DASHBOARD_USER})"
echo "    Prometheus Metrics: http://localhost:9283/metrics"
ceph -s

# Keep container alive and forward signals
trap 'echo "Stopping Ceph daemons..."; pkill -TERM ceph-osd ceph-mgr ceph-mon || true; exit 0' SIGINT SIGTERM
tail -f /dev/null &
wait $!
