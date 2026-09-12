# Quickstart

Sixty seconds from a bare Linux machine to a checkpointed, restorable workload — then
the same commands move it to another machine. Depth lives in the linked docs; this page
is only the path.

## 1. Install (one command)

```bash
curl -fsSL https://github.com/Fed-Labs/shiftgate/releases/latest/download/install.sh | bash
```

This verifies the release's SHA-256 before installing anything, puts `shiftgate`,
`shift-agent`, and `shift-control` in `/usr/local/bin`, installs CRIU through your
package manager when it is missing, and enables the agent as a systemd service with a
local-only configuration (no remote listener, so no TLS material). It adds you to the
`shift` group so the CLI works unprivileged after one re-login (or `newgrp shift`).

From a repository checkout instead: `./install.sh` (builds from sources; same layout).

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

`--leave-running` freezes nothing: the snapshot is taken while the server keeps
answering. Each checkpoint is a full, independently restorable image of the process and
its filesystem root, compressed (zstd), encrypted (AES-256-GCM), and split into
content-addressed chunks — repeated checkpoints of unchanged state store almost nothing
new.

Restore it — the process resumes where the snapshot caught it:

```bash
shiftgate restore CHECKPOINT_ID           # from `checkpoint list`
```

## 3. Make it automatic

```bash
shiftgate workload policy demo --every 5m --keep 3
```

The agent now snapshots the workload every five minutes without stopping it and prunes
the oldest beyond three. Interval floor is 10 seconds; the schedule is measured from the
newest checkpoint, so it survives agent restarts. Every periodic snapshot is full-size —
see [limitations](limitations.md) for the storage trade-off. `workload policy demo --off`
removes it.

## 4. Move it to another machine

Install on the second machine the same way. The two agents then need mutual TLS: the
migration channel is machine-to-machine, so a plain HTTP listener is refused.

Once, on any machine, create a private CA and issue each machine a certificate:

```bash
openssl genrsa -out agents-ca.key 4096
openssl req -x509 -new -key agents-ca.key -days 3650 -subj "/CN=SHIFT Agents CA" -out agents-ca.pem
# per machine (repeat with machine-b.key / machine-b.crt on the other host):
openssl genrsa -out machine-a.key 4096
openssl req -new -key machine-a.key -subj "/CN=machine-a" -out machine-a.csr
openssl x509 -req -in machine-a.csr -CA agents-ca.pem -CAkey agents-ca.key \
    -days 825 -out machine-a.crt
```

On the destination, put its key and certificate plus the CA at the paths the example
config uses, enable the remote listener, and restart:

```bash
sudo install -d -m 0750 /etc/shift/tls
sudo install -m 0644 machine-b.crt /etc/shift/tls/agent-cert.pem
sudo install -m 0600 machine-b.key /etc/shift/tls/agent-key.pem
sudo install -m 0644 agents-ca.pem /etc/shift/tls/agents-ca.pem
# edit /etc/shift/agent.json: set "remote_listen": "tcp://0.0.0.0:8443" and the
# "tls" block (certificate_file, private_key_file, client_ca_file, peer_ca_file)
# as in deployments/systemd/agent.json.example
sudo systemctl restart shift-agent
```

Read the destination's machine id — migrations pin it, so a mistyped address cannot
silently land on the wrong host:

```bash
shiftgate machines                            # on the destination: MACHINE <id>
```

Then, on the source, ask whether the migration would work before anything moves:

```bash
shiftgate migrate demo --dry-run --to https://DESTINATION:8443 --machine-id DESTINATION_ID
```

The dry run reaches the destination over the same peer channel, runs the same
compatibility check a real migration would, and prints both machines, the network plan,
and every rejection with its remedy — an incompatibility is exit code 3, not a surprise
halfway through a migration. Nothing is frozen, moved, or recorded.

Move it — cold (the workload is stopped for the transfer) or live (pre-copy passes move
memory while it keeps running, and only the final dump freezes it):

```bash
shiftgate migrate demo --to https://DESTINATION:8443 --machine-id DESTINATION_ID
shiftgate migrate demo --mode live --pre-copy-passes 4 --to https://DESTINATION:8443 --machine-id DESTINATION_ID
```

When it commits, the workload is running on the destination with its state intact and the
source is stopped — never deleted. If anything fails on the way, the source resumes; a
migration never strands the application. What actually happened to the workload's network
is written to `.shift/migration-status.json` in its root.

## 5. Next

- [CLI reference](cli.md) — every command, exit codes, JSON output.
- [Benchmarks](benchmarks.md) — measured compression, checkpoint, and migration numbers
  with the methodology to reproduce them.
- [Architecture](architecture.md) and [Limitations](limitations.md) — how it works, and
  exactly what it does not do.

