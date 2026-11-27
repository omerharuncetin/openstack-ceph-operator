FROM quay.io/ceph/ceph:v19

COPY bootstrap.sh /usr/local/bin/bootstrap-ceph.sh
COPY drills/openstack-rbd-drill.sh /drills/openstack-rbd-drill.sh
RUN chmod +x /usr/local/bin/bootstrap-ceph.sh /drills/openstack-rbd-drill.sh

# Ports: 6789/3300 (MON v1/v2), 8443 (Dashboard REST API), 9283 (Prometheus exporter)
EXPOSE 6789 3300 8443 9283

ENTRYPOINT ["/usr/local/bin/bootstrap-ceph.sh"]
