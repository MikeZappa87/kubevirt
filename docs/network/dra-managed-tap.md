# DRA pod-link networking (prototype)

This implementation connects a DRA-provisioned pod netdev to a guest through
KubeVirt's existing bridge or TAP attachment paths. No NAD or Multus is required.

Supported bindings:
- Built-in `bridge: {}`: KubeVirt creates the bridge and TAP and uses its existing
  DHCP behavior when the pod interface has IP configuration.
- Plugin `domainAttachmentType: managedTap`: KubeVirt creates the bridge and TAP.
- Plugin `domainAttachmentType: tap`: the driver/plugin must provide a usable
  TAP or macvtap, including connectivity and QEMU access permissions. A veth
  alone is insufficient; KubeVirt does not build a bridge for this binding.
- Other plugins keep their own device discovery; they are not required to report
  a pod interface (for example, PCI or socket-based bindings).

For managedTap, enable `NetworkDevicesWithDRA` and register a binding:

```yaml
# Merge into the existing KubeVirt CR spec.configuration
network:
  binding:
    dra-managed-tap:
      domainAttachmentType: managedTap
```

Use the binding with a native claim-backed network (replace the binding block
with `bridge: {}` to use the built-in bridge binding):

```yaml
# Relevant fields under VirtualMachine spec.template.spec
resourceClaims:
  - name: network
    resourceClaimTemplateName: dra-poc
domain:
  devices:
    interfaces:
      - name: default
        masquerade: {}
      - name: vlan110
        model: virtio
        binding:
          name: dra-managed-tap
networks:
  - name: default
    pod: {}
  - name: vlan110
    resourceClaim:
      claimName: network
      requestName: net
```

Keep the rest of the VM definition (disks, memory, volumes, architecture).
The driver must create the netdev in the launcher pod network namespace before
network setup and publish its name in ResourceClaim
`status.devices[].networkData.interfaceName`. The controller matches this status
by the allocated device's driver, pool, and device name, after selecting the
requested allocation. Missing allocation/status is retried before VMI handoff.
Template claims are resolved from the launcher pod's resourceClaimStatuses;
pre-existing claims are supported as well.

The interface name is saved as VMI status.interfaces[].podInterfaceName, so
virt-handler and virt-launcher share the same identity. TAP names are derived
from the VMI network name and do not collide with the default network's tap0.
The normal managedTap setup renames the veth to `<interface>-nic` and keeps a
dummy interface under the original name. Driver cleanup must tolerate this.

Scope and limits:

- Cold-start only; hotplug and migration are not implemented by this change.
  Do not advertise migration support in the binding registration.
- Exactly one allocated device per network request (including selected
  prioritized subrequests). Separate requests can provide multiple networks.
- Bridge and managedTap interface names are restricted to 11 bytes to leave
  room for generated bridge and renamed-link names. Tap permits 15 bytes. `net1` and `vlan110` both work.
- Guest addressing is separate. The claim's networkData.ips are not copied into
  the guest. Existing pod IPs retain managedTap's normal dummy-interface
  behavior; do not assume they have been delegated to the guest.
- Unit tests cover the mapping and generated setup; live driver/NRI ordering,
  guest traffic, and driver cleanup still need a cluster smoke test.

Deployment requires rebuilt virt-controller, virt-handler, and virt-launcher
images from this checkout; YAML alone cannot enable this implementation on an
unmodified release. Recreate the VMI with the native network declaration after
installing those components. Built-in bridge admission also requires a rebuilt
virt-api.
