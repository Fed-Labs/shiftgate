# Troubleshooting

Symptoms, causes, and fixes — in the order operators actually hit them.
Every diagnosis below starts with a command, because every answer below is
something the system reports about itself.

## First: `shiftgate doctor`

```bash
shiftgate doctor
```

It runs the same checks the agent exposes at `GET /v1/doctor`: CRIU
installed and healthy, kernel capabilities, state directory writable,
chunk store reachable, object store (when configured) reachable. Start here
before reading anything else; most "migration failed" reports end in one
of the doctor checks.

## "CRIU is not installed" / checkpoint fails

- `criu --version` — the binary must exist on the agent's PATH. On
  Debian/Ubuntu: `apt install criu`.
- `sudo criu check` — reports the kernel capabilities CRIU needs. On
  containers and some VMs, missing capabilities (e.g. `CHECKPOINT_RESTORE`,
  `POSBARB`) cannot be fixed from userspace; the agent will never fake a
  checkpoint there, and that is the honest answer.
- Run the agent as root. CRIU dump/restore requires it.

## Migration fails with DESTINATION_UNREACHABLE

The source agent could not reach the destination's peer URL.

- From the source machine, test the URL the migration was given:
  `curl -k https://DEST_HOST:PORT/v1/peer/machine` — it must answer JSON.
- Check `remote_listen` on the destination agent and any firewall between
  the machines. Peer traffic is TLS on that port; the local Unix socket is
  not part of it.
- In production the peer listener requires mutual TLS: the source needs a
  client certificate the destination's client CA trusts. In development
  (`insecure_development`) both sides self-issue — never enable that off
  localhost.

## Migration fails with DESTINATION_INCOMPATIBLE

The compatibility report (visible on the migration record and in the
desktop client) lists every issue with a severity. Common ones:

- Architecture or kernel mismatch — migration cannot proceed; this is the
  check doing its job. Move with a cold file transfer or rebuild for the
  target.
- Missing GPU — depends on the workload's `device_policy`
  (`reject_incompatible` blocks, `warn_incompatible` proceeds and records
  the gap).
- CRIU missing on the destination — a live migration needs it; a cold
  migration of the filesystem can still run.

## Migration fails mid-transfer; what happens to my workload?

Nothing is lost. The stage machine persists every transition; any failure
rolls back the destination (the reservation is released) and **resumes the
source workload**. The migration record shows `ROLLED_BACK` with
`source_preserved: true` and the failure code. Read the event log:

```bash
shiftgate status MIGRATION_ID
```

If the record shows `FAILED` with "rollback requires operator
intervention", the rollback itself hit an error (dead peer, resume
failure) — the source root is still on disk; the failure reason names the
step.

## Restore fails but the checkpoint exists

Run the restore and read the error; it names the failing step:

- Chunk integrity errors (AEAD authentication failed) mean the chunk store
  is corrupted. This is a fail-closed by design — restore never proceeds
  over unverifiable state. Re-cover from the object-store mirror:
  `shiftgate checkpoint mirror CHECKPOINT_ID` is idempotent and re-fetches
  missing objects.
- Port reservation errors mean another workload on this machine holds the
  declared host port; the restore refuses before touching anything.
- "no network layer configured" — the workload declares ports but the
  agent has no network layer; the restore refuses rather than restoring a
  silently unreachable service.

## Agent will not start

- `state_dir must be an absolute path` — fix the configuration.
- `the local control API must use a Unix socket in production` — set
  `listen` to `unix:///run/shift/agent.sock` (TCP local listeners are a
  development-mode feature).
- Certificate errors on `remote_listen` — production peer listeners require
  `tls.certificate_file`, `tls.private_key_file`, and `tls.client_ca_file`.

## Desktop client cannot reach the agent

- The socket path in Settings must match the agent's `listen` path.
  `SHIFT_AGENT_ENDPOINT` overrides it at startup.
- Verify the socket exists and is readable by your group: the agent creates
  it mode 0660 with group ownership per its service setup.
- The client's Health indicator polls every 10 s; give it one cycle after
  fixing.

## Control plane: 401 on every request

Sessions are short-lived by design. The CLI/desktop client refresh
automatically; a standalone script hitting the API must implement the same
login → access token → refresh rotation flow. A 401 from a long-lived
integration usually means its refresh token was rotated out by another
client using the same session.

## Update installed but agent still reports the old version

The staged binary is swapped in only when the agent can restart safely —
not during a migration, restore, fork, or incoming transfer. Check:

```bash
shiftgate update status
```

`pending_restart` means exactly that; it swaps at the next safe point.
`shiftgate update rollback` undoes a completed swap; the undone version is
blocked from reinstalling until `shiftgate update unblock VERSION`.

## Where the facts live

- `GET /v1/migrations/{id}` — every stage transition, failure code, byte
  count, and event for one migration.
- `GET /v1/restores` — restore transactions with per-state timestamps and
  the error that stopped a failed one.
- `GET /v1/doctor` — machine health from the agent's own point of view.
- Agent logs (slog) at `debug` level narrate the migration stage machine
  step by step.
