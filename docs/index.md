---
layout: home
hero:
  name: ovn-network-agent
  text: Event-driven networking for OVN
  tagline: A real-time daemon that watches OVN databases via OVSDB and synchronizes Floating IP routes, BGP announcements, and DNAT port forwards on gateway nodes.
  actions:
    - theme: brand
      text: First agent on a test host
      link: /tutorials/first-agent
    - theme: alt
      text: Configuration reference
      link: /reference/configuration
    - theme: alt
      text: View on GitHub
      link: https://github.com/osism/ovn-network-agent
features:
  - title: Event-driven reconcile
    details: Connects to OVN Southbound and Northbound via OVSDB IDL, reacts to Port_Binding, Chassis, NAT, and Logical_Router changes in real time, with a periodic safety-net reconcile.
  - title: Gatewayless provider networks
    details: Invents a virtual "magic gateway" and writes default routes plus static MAC bindings into OVN NB so SNAT reply traffic exits the logical router without a physical upstream gateway.
  - title: BGP /32 announcement
    details: Installs kernel /32 routes, IP rules, and FRR static routes in a dedicated VRF so FRR announces each FIP and SNAT IP to the external fabric.
  - title: Port forwarding (DNAT)
    details: Forwards anycast VIPs to internal backends with sticky multi-backend hashing, conntrack-based return routing, and source-selective masquerade for FIP and router-SNAT hairpin.
  - title: Hitless gateway drain
    details: Lowers Gateway_Chassis priority to zero on SIGINT/SIGTERM so OVN migrates traffic before BGP withdrawal, eliminating the failover gap on rolling upgrades.
  - title: Prometheus metrics
    details: Exposes reconcile counters, drain durations, OVN connection state, and stale-chassis cleanup events on an optional HTTP endpoint, with suggested alert rules.
---
