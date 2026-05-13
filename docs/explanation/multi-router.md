# Multi-router support

The agent automatically discovers **all** chassisredirect port bindings in
the OVN Southbound database and determines which logical routers are active
on the local chassis. Only the FIPs/SNATs belonging to locally-active routers
are managed.

This means a single agent instance handles the common multi-router scenario
where OVN distributes different routers across different gateway nodes:

```
net-01 runs agent → sees router-A, router-D active locally → routes their FIPs
net-02 runs agent → sees router-B, router-E active locally → routes their FIPs
net-03 runs agent → sees router-C, router-F active locally → routes their FIPs
```

On failover (e.g. router-A moves from net-01 to net-02), the agent on net-01
removes router-A's routes and the agent on net-02 adds them.

To restrict the agent to a single router (legacy behavior), set
`gateway_port` to a specific chassisredirect port name.
