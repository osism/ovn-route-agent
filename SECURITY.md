# Security Policy

We take the security of `ovn-network-agent` seriously. This document explains
how to report vulnerabilities privately, which versions receive fixes, and
what response timeline you can expect.

## Reporting a vulnerability

**Please do not report security issues through public GitHub issues, pull
requests, or discussions.**

There are two private channels:

1. **GitHub Security Advisories (preferred).** Use
   [Report a vulnerability](https://github.com/osism/ovn-network-agent/security/advisories/new)
   to open a private advisory. This is the fastest path and lets us
   collaborate on a fix and a coordinated disclosure in one place.
2. **Email.** Send the report to **<support@osism.cloud>**. Please include
   `ovn-network-agent` in the subject line.

A useful report includes:

- A description of the issue and its impact.
- The affected version(s) (`ovn-network-agent --version` output, or the
  commit hash if you built from source).
- Steps to reproduce, ideally a minimal proof of concept.
- Any known mitigations or workarounds.
- Whether you intend to publish your own write-up, and on what timeline.

If you would like to encrypt your report, request our PGP key in your
initial mail and we will send it back before you share details.

## Response timeline

We aim for the following service levels for reports received through the
channels above:

| Stage                     | Target                                      |
| ------------------------- | ------------------------------------------- |
| Acknowledge receipt       | within **3 business days**                  |
| Initial triage / severity | within **7 business days**                  |
| Fix or mitigation plan    | within **30 days** for high/critical issues |
| Coordinated disclosure    | once a fix or workaround is available       |

These are targets, not guarantees. We will keep you updated if a report
needs longer, and we are happy to coordinate disclosure dates with you and
with downstream distributors.

## Supported versions

`ovn-network-agent` has **not yet reached `1.0.0`**. During the `0.x`
phase, no release line is officially supported with backported security
fixes. Fixes land on `main` and ship with the next release; the only way
to consume them is to upgrade to the latest tagged version (or build from
`main`).

| Version            | Supported                              |
| ------------------ | -------------------------------------- |
| `>= 1.0.0`         | :white_check_mark: (planned, not yet)  |
| `0.x` (all)        | :x: — upgrade to the latest release    |

Once `1.0.0` is released, this table will be updated with the actual
supported minor lines and their end-of-life policy.

## Scope

In scope:

- The `ovn-network-agent` binary and its Go source in this repository.
- Packaging artefacts under `packaging/` and `nfpm.yaml`.
- The systemd unit and default configuration shipped with releases.

Out of scope:

- Vulnerabilities in OVN, OVS, FRR, the Linux kernel, or other upstream
  dependencies — please report those to the respective projects. We are
  still happy to hear about them if they affect how the agent should be
  deployed.
- Issues that require an attacker who already has root on the host where
  the agent runs.
- Findings from automated scanners without a demonstrated impact on the
  agent.

## Credit

We are glad to credit reporters in the advisory and release notes. Let us
know in your report whether you want to be named and how.
