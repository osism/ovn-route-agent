# Create a gatewayless provider network

For background on *why* the agent needs a virtual gateway and *how* it
constructs one, see
[Gatewayless provider networks](../explanation/gatewayless-networks). This page
covers the OpenStack-side configuration that triggers the gatewayless path.

The key difference to a normal provider network is that the subnet has **no
gateway IP** and the **last usable address** (`.254` in this example) is kept
free — the agent will use it as the virtual gateway.

## Ansible (openstack.cloud collection)

```yaml
- name: Create public network
  openstack.cloud.network:
    cloud: admin
    state: present
    name: public
    external: true
    provider_network_type: flat
    provider_physical_network: physnet1
    mtu: 1500

- name: Create public subnet (gatewayless)
  openstack.cloud.subnet:
    cloud: admin
    state: present
    name: subnet-public-001
    network_name: public
    cidr: 198.51.100.0/24
    enable_dhcp: false
    allocation_pool_start: 198.51.100.1
    allocation_pool_end: 198.51.100.253
    # no gateway_ip → OpenStack sets disable_gateway_ip: true
```

## OpenStack CLI equivalent

```bash
openstack network create --external --provider-network-type flat \
  --provider-physical-network physnet1 --mtu 1500 public

openstack subnet create --network public --subnet-range 198.51.100.0/24 \
  --no-dhcp --allocation-pool start=198.51.100.1,end=198.51.100.253 \
  --gateway none subnet-public-001
```

Note that the allocation pool ends at `.253` — address `.254` is reserved for
the agent's virtual gateway. The `--gateway none` flag (or omitting
`gateway_ip` in Ansible) tells OpenStack not to assign a real gateway, which
is exactly what triggers the gatewayless scenario that the agent handles.
