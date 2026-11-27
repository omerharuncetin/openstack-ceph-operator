# CEPH 

Ceph is a storage abstraction software. It abstracts file, object, and disk storages. It places OSD to each disk and one can scale up to thousands of disks.

## CEPH Components
CEPH has multiple components to manage this infrastructure.

### Monitors

- Maintains cluster membership and state
- Responsible for consensus in distributed environment
- Monitors must be odd number count

### OSD (Object Storage Daemon)

- It is on each Disk
- OSD serve objects to clients
- There can be 10k per cluster
- Peered to other OSDs for replication

### Manager

- Responsible for monitoring, orchestration and plugin system

### Metadata Server

- Manages file metadata

## Storing Data
Data in CEPH has three sections

ID, Binary Data and Metadata

- ID: Id
- Binary Data: Actual data
- Metadata: Metadata related to binary data

## CRUSH Algorithm
- It is a pseudo-random placement algorithm
- It does uniform distribution (statically)
- Flexible rules
- - Adjustable replication
- - Weighting
- - Infra topology based (replicates to a different region)

Formula: hash(name) % placement group


## Using CEPH on a cluster

Rook can be used to operate the CEPH. It is the operator for the cluster. It created crds, and necessary pods like operator, and discovers. Discover discovers each node and disks on them. Rook also has dashboards and many more ceph related component.


## Zapping
Zapping means erasing the disk. If a storage already has a filesystem, then CEPH rejects it.

first list devices

```ceph orch device ls```

then zap it

``` ceph orch device zap <host> <path> --force```

After that your disk will be ready for CEPH.
