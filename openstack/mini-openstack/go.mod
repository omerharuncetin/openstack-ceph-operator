module github.com/omerharuncetin/openstack-ceph-operator/openstack/mini-openstack

go 1.26.11

require github.com/omerharuncetin/openstack-ceph-operator/ceph/ceph-client v0.0.0

require github.com/gophercloud/gophercloud/v2 v2.16.0

replace github.com/omerharuncetin/openstack-ceph-operator/ceph/ceph-client => ../../ceph/ceph-client
