# CLI

The `shiftgate` client talks to the local agent and emits either tables or JSON:

```sh
shiftgate doctor
shiftgate machines
shiftgate workload create demo --path "$PWD" -- /usr/bin/python3 -m http.server 8080
shiftgate workload start demo
shiftgate checkpoint create demo --leave-running
shiftgate checkpoint mirror CHECKPOINT_ID
shiftgate fork demo --name demo-experiment
shiftgate fork demo --activate --root /srv/demo-b
shiftgate fork list
shiftgate migrate demo --to https://workstation.example:8443 --machine-id DESTINATION_ID
shiftgate status
shiftgate update status
shiftgate update check
shiftgate update apply --version 1.4.2
shiftgate update rollback
shiftgate update block 1.4.3 --reason "restarted the workload under load"
shiftgate update unblock 1.4.3
```

Use `--agent unix:///path/to/agent.sock`, `--json`, and `--timeout` to override defaults.
Exit codes distinguish usage errors (2), failed prerequisites or transport (3), missing
resources (4), failed/rolled-back migrations (5), and deferred updates (6).
`shiftgate completion bash|zsh|fish` generates shell completion snippets.

## Control-plane commands

The same binary manages a control-plane session. Pass `--control-url` (or set
`SHIFT_CONTROL_URL`) to point at the control plane; `--token-store` overrides
where the session is stored (default `~/.config/shift/cli-session.json`, 0600
in a 0700 directory). The password is prompted on the terminal with echo
disabled and is never accepted as a command-line flag, so it cannot land in
shell history or process listings; piping stdin also works for scripts.

```sh
shiftgate --control-url https://control.example.com login --email operator@example.com
shiftgate whoami
shiftgate plans
shiftgate logout
```

When the control plane has an OIDC issuer configured, `login --sso` authenticates through
it instead of a password: the CLI starts a loopback listener, asks the control plane for
the issuer's authorization URL, opens the browser (or prints the URL on a headless
machine), and redeems the one-time authorization code the listener receives. The PKCE
verifier never leaves the process and the code travels only over the loopback interface.
A password is never read, so `login --sso` also works for domains whose organizations
enforce single sign-on — password login for those domains is refused outright.

```sh
shiftgate login --sso --email operator@company.example.com
```

With a control plane configured, `shiftgate machines` and `shiftgate workloads` show
the fleet view instead of the local machine. The organization defaults to the
first one the account belongs to; pass `--org ID` before a subcommand to
choose another.

```sh
shiftgate machines                          # fleet view when --control-url is set
shiftgate machines register --machine-id workstation --name Workstation --agent-url https://workstation:8443
shiftgate machines capability cuda_version --kind number --minimum 12
shiftgate fleet workloads
shiftgate fleet migrations                  # list jobs
shiftgate fleet migrations MIGRATION_ID     # one job with its event stream
shiftgate fleet checkpoints [WORKLOAD]
shiftgate fleet audit
shiftgate fleet api-keys
shiftgate fleet api-keys create --name ci --scopes machines,workloads
shiftgate fleet api-keys revoke KEY_ID
shiftgate fleet retention                   # show retention windows
shiftgate fleet retention --audit-days 90 --checkpoint-days 30
shiftgate fleet sso                         # show SSO enforcement state
shiftgate fleet sso --enforce example.com   # claim a domain for single sign-on
shiftgate fleet sso --disable               # release the claim
shiftgate fleet entitlement
shiftgate fleet usage --from 2026-08-01 --to 2026-08-28
```

`shiftgate machines capability NAME` is a fleet query through the capability
projection the control plane maintains alongside each machine's JSONB
document: `--kind number|text|boolean` selects the comparison (numbers also
take `--minimum`), and only scalar capabilities are queryable — objects and
arrays stay in the document. Retention windows are in days; an omitted flag
leaves its window unchanged, and `0` is the deliberate keep-forever policy.
The control plane enforces the windows on a background sweep (configurable
via `retention_sweep_interval`, default hourly): audit rows past their window
are deleted, checkpoints past theirs are marked deleted while their lineage
rows survive, and storage objects already soft-deleted are purged after their
grace period.

`shiftgate fleet sso` manages single sign-on enforcement (admin role; the control
plane must have an OIDC issuer configured). Enforcing SSO claims an email
domain: the domain's accounts then authenticate with `shiftgate login --sso`,
password login and self-service registration are refused for them, and a
federated login automatically joins the organization as a viewer. One
organization can claim a domain at a time, and disabling releases the claim
rather than parking it.

## Marketplace

Compute offers, placements, and reservations:

```sh
shiftgate marketplace inventory                     # everything schedulable now
shiftgate marketplace offers                        # this organization's offers
shiftgate marketplace publish --machine workstation --cpu 8 --memory 32GiB \
    --region eu-central --country DE --visibility organization
shiftgate marketplace withdraw --offer OFFER_ID
shiftgate marketplace place --workload demo --cpu 4 --memory 8GiB --checkpoint CHECKPOINT_ID
shiftgate marketplace reserve --offer OFFER_ID --workload demo --cpu 4 --memory 8GiB
shiftgate marketplace reservations
shiftgate marketplace commit --reservation RESERVATION_ID    # migration landed
shiftgate marketplace release --reservation RESERVATION_ID   # capacity returned
shiftgate marketplace fail --reservation RESERVATION_ID --reason "restore failed"
```

A placement ranks destinations and explains every rejection without holding
anything; a reservation runs the real scheduler server-side inside the store's
locked transaction, so a hold can never be granted on terms a placement would
refuse, and it expires (or is released) rather than pinning capacity forever.

The `shiftgate update` subcommands manage this machine's agent updates through the local API:
`status` shows the running and pending versions, available and refused releases, preserved
backups, blocked versions, and history; `check` consults the release feed immediately;
`apply` installs the current selection or one named version; `rollback` reinstalls a
preserved binary and blocks the version it undid; `block` and `unblock` manage the local
block list. A release is only installed after its signatures verify against the configured
key ring and the staged binary passes the `--version` self-check, and an install that
would interrupt a migration, restore, fork, or incoming transfer exits 6 to be retried
later.
