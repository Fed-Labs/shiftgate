# Quickstart

Sixty seconds from a bare Linux machine to a checkpointed, restorable workload — then
the same commands move it to another machine. Depth lives in the linked docs; this page
is only the path.

## 1. Install (one command)

```bash
curl -fsSL https://github.com/Fed-Labs/shiftgate/releases/latest/download/install.sh | bash
```

This verifies the release's SHA-256 before installing anything, puts `shiftgate`,
`shift-agent`, `shift-control`, the offline `shift-gc`, the backup-drill
verifier `shift-verify`, and the relay `shift-relay` in `/usr/local/bin`,
installs CRIU through your package manager when it is missing, and enables the
agent as a systemd service with a local-only configuration (no remote listener,
so no TLS material). It creates the `shift` group and adds you to it so the CLI
works unprivileged after one re-login (or `newgrp shift`).

From a repository checkout, `sudo ./install.sh` builds from sources and reaches
the same end state — same binaries, same local-only service, same group
enrollment (no toolchain? use the release path above). Both installers take
`--no-service` for binaries only and `--prefix DIR` for another location.

Check the machine can really checkpoint — this probes CRIU and the kernel, it does not
print an optimistic yes:

```bash
shiftgate doctor
```

## 2. First workload, first checkpoint (thirty seconds)

```bash
mkdir -p ~/demo && cd ~/demo
shiftgate workload create demo --path "$PWD" -- python3 -m http.server 8080
shiftgate workload start demo
curl -s localhost:8080 | head -1          # it is serving
shiftgate checkpoint create demo --leave-running
shiftgate checkpoint list
```

`--leave-running` (the default) resumes the server automatically when the dump
finishes: the workload is frozen only for the CRIU dump itself, and the service
keeps answering except during that window — freeze time grows with memory size
([benchmarks](benchmarks.md) report measured ones). Each checkpoint is a full,
independently restorable image of the process and its filesystem root, compressed
(zstd), encrypted (AES-256-GCM), and split into content-addressed chunks — repeated
checkpoints of unchanged state store almost nothing new.

Restore it — the process resumes where the checkpoint caught it:

```bash
shiftgate restore CHECKPOINT_ID           # from `checkpoint list`
```

## 3. Make it automatic

```bash
shiftgate workload policy demo --every 5m --keep 3
```

Requires Pro or above when signed in to the platform — periodic checkpoint
policies are not available on the free tier (the CLI reports
`PLAN_REQUIRED_PERIODIC_POLICY`). Local-only use without a control-plane
session is not gated.

The agent now checkpoints the workload every five minutes without stopping it and prunes
the oldest beyond three. Interval floor is 10 seconds; the schedule is measured from the
newest checkpoint, so it survives agent restarts. Every periodic checkpoint is full-size —
see [limitations](limitations.md) for the storage trade-off. `workload policy demo --off`
removes it.

## 4. Move it to another machine

SHIFT has two remote connection modes:

- **Direct peer mode**: the source dials the destination's public HTTPS peer
  endpoint over TLS 1.3 mutual TLS. This is the lowest-latency path and works
  across the internet when DNS, firewall, and NAT forwarding are configured.
- **Relay mode**: a self-hosted MVP ships (`shift-relay`, see
  [Relay](relay.md)) but is not offered on the hosted platform in this
  release, and agent-side relay route selection is not enabled by default —
  direct peer mode is the supported remote path today. Hosts behind
  symmetric NAT need that authenticated relay deployment; SHIFT never falls
  back to plaintext or exposes the local Unix API publicly.

### Direct internet peer setup

On the destination, use a stable DNS name such as `gpu-west.example.com`,
forward TCP 8443 to the agent, and issue a CA-signed certificate whose SAN
contains that DNS name. The client certificate must be enrolled for the
agent's machine identity; do not use the CN-only/RSA example from older docs.
The peer listener is mTLS-only and serves transfer endpoints, never the local
workload API.

Configure the destination with:

```json
{
  "remote_listen": "tcp://0.0.0.0:8443",
  "tls": {
    "certificate_file": "/etc/shift/tls/agent-cert.pem",
    "private_key_file": "/etc/shift/tls/agent-key.pem",
    "client_ca_file": "/etc/shift/tls/agents-ca.pem",
    "peer_ca_file": "/etc/shift/tls/agents-ca.pem"
  }
}
```

Allow only the source/standby addresses through the firewall. Install the same
private CA trust on the source and destination. Verify the remote machine
identity on the destination before migrating:

```bash
shiftgate machine
```

Save a friendly alias on the source. The URL is public/reachable; it is not a
local `127.0.0.1` address:

```bash
shiftgate connect gpu-west \
  --to https://gpu-west.example.com:8443 \
  --machine-id DESTINATION_MACHINE_ID
shiftgate connections
```

Then use the alias. The CLI fills the destination URL, machine identity, and
TLS server name from the saved profile:

```bash
shiftgate migrate demo --to gpu-west --preflight
shiftgate migrate demo --to gpu-west
shiftgate migrate demo --to gpu-west --mode live --pre-copy-passes 4   # paid plans; free tier is cold-only
```

For a one-off direct migration without saving an alias:

```bash
shiftgate migrate demo \
  --to https://gpu-west.example.com:8443 \
  --machine-id DESTINATION_MACHINE_ID \
  --server-name gpu-west.example.com
```

The source performs the compatibility check before state moves. If the
internet route, mTLS certificate, hostname, or machine identity is wrong, the
migration fails before the workload is frozen and the source remains running.
`--preflight` is an alias for `--dry-run`.

## 5. Next

- [CLI reference](cli.md) — every command, exit codes, JSON output.
- [Benchmarks](benchmarks.md) — measured compression, checkpoint, and migration numbers
  with the methodology to reproduce them.
- [Architecture](architecture.md) and [Limitations](limitations.md) — how it works, and
  exactly what it does not do.

