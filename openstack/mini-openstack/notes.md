# OPENSTACK

OpenStack is an open-source cloud platform that manages compute, storage, and networking resources through REST APIs. Each capability is handled by its own service, and services authenticate with each other through Keystone.

## OpenStack Components

### Keystone

- Handles authentication and authorization (users, projects, domains, roles)
- Issues scoped tokens (`X-Subject-Token` / `X-Auth-Token`)
- Provides the service catalog so clients can discover endpoint URLs for every other service

### Nova

- Manages VM lifecycle (create, resize, reboot, live migrate, delete)
- Schedules VMs onto compute nodes based on CPU, RAM, disk, and placement rules
- Talks to hypervisors (usually KVM/QEMU through libvirt)
- Uses flavors to define VM vCPU, RAM, and disk sizes

### Neutron

- Provides virtual networking as a service
- Manages L2 networks, subnets, L3 routers, floating IPs, security groups, and ports
- Allocates MAC addresses and IP addresses from subnet CIDRs
- Runs on top of a backend plugin such as OVN (Open Virtual Network) or Linux Bridge

### Cinder

- Provides persistent block storage volumes to VMs
- Supports snapshots, cloning, volume types, and online volume extension
- Uses storage drivers to connect to backends like Ceph RBD, LVM, or iSCSI/FC SANs

### Glance

- Stores and serves OS disk images (raw, qcow2, ISO)
- Supports backends such as local filesystem, Swift, or Ceph RBD
- When backed by Ceph RBD with raw images, Cinder and Nova can clone images directly with copy-on-write

### Horizon

- Web dashboard that talks to the OpenStack APIs on behalf of users and admins

## Booting a VM

When a user launches a VM, the services work together in order:

1. Client authenticates with Keystone and gets a token and service catalog.
2. Nova validates the flavor and image (Glance) or boot volume (Cinder).
3. Placement and Nova Scheduler pick a compute host with enough capacity.
4. Neutron creates or binds a port on the tenant network and assigns an IP and MAC address.
5. Cinder attaches the block volume (if booting from volume).
6. Nova Compute on the chosen host defines and starts the VM through libvirt.

## Networking with OVN

OVN replaces legacy Neutron L2, L3, and DHCP agents with a database-driven SDN control plane.

- Neutron writes logical networks, routers, and ACLs into the OVN Northbound DB
- `ovn-northd` translates them into logical flows in the OVN Southbound DB
- `ovn-controller` on each compute host programs local Open vSwitch (OVS) bridges
- East-west traffic between hosts uses Geneve overlay tunnels
- Each port is bound to a compute host chassis via `binding:host_id`

## Live Migration

Live migration moves a running VM from one compute host to another without shutting it down.

- With shared storage (like Ceph RBD), only RAM pages are copied across hosts (`block_migration=false`)
- With local disks, both disk blocks and RAM must be copied (`block_migration=true`)
- After memory cutover, Neutron rebinds the VM port to the destination host chassis

## Deploying OpenStack

In production, OpenStack services are usually deployed in containers using tools like Kolla-Ansible, OpenStack-Helm, or OpenStack-Ansible, with HAProxy/Keepalived in front of API endpoints, MariaDB Galera for state, and RabbitMQ for RPC between services.

## Common CLI Commands

List images, networks, volumes, and servers:

```bash
openstack image list
openstack network list
openstack volume list
openstack server list
```

Create a volume from an image and boot a VM:

```bash
openstack volume create --image ubuntu-24.04 --size 20 boot-vol-1
openstack server create --flavor m1.small --volume boot-vol-1 --network app-net web-1
```

Live migrate a VM to another host:

```bash
openstack server migrate --live-migration --host compute-02 web-1
```
