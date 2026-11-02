# Authenticated outbound relay (MVP)

`shift-relay` is an opaque byte-stream relay for future outbound-only agent
sessions. It never exposes the local agent API and never parses peer HTTP,
checkpoint manifests, keys, or payloads. The inner agent-to-agent TLS session
must remain end-to-end.

## Prerequisites

The relay refuses to start without its full authorization surface (there is
no insecure fallback):

1. **A durable leg store** — a PostgreSQL database (`--database-url` or
   `SHIFT_DATABASE_URL`). One-use tickets are enforced by claim/consume rows
   here; without it a captured ticket could be replayed. The database is the
   schema `shift-control` migrates, so point the relay at the fleet database.
2. **The machine client CA** (`--client-ca`) — the PEM bundle whose client
   certificates the fleet's agents present. Every relay leg must show a
   certificate this CA signed, so the relay can bind legs to certified
   machine identities (the same CA as agents' `tls.client_ca_file`).
3. **A ticket audience** (`--audience` or `SHIFT_RELAY_AUDIENCE`) — tickets
   are bound to the relay that will honor them; a ticket minted for another
   audience is refused. The control plane stamps its `relay_endpoint`
   (`SHIFT_RELAY_ENDPOINT`) into every ticket as that audience, so the relay's
   audience must be the exact same string — byte for byte. Because agents
   dial the endpoint too, the value is the relay's dialable URL
   (`https://host:port`), not an arbitrary label.
4. **A ticket signing secret** (`SHIFT_RELAY_SIGNING_KEY`, 32+ bytes) —
   shared with the control plane's `relay_signing_key`; rotating it
   invalidates in-flight tickets only.

## Start the relay

```bash
export SHIFT_RELAY_SIGNING_KEY="$(openssl rand -hex 32)"
export SHIFT_DATABASE_URL="postgres://shift:...@db.internal:5432/shift?sslmode=verify-full"
export SHIFT_RELAY_AUDIENCE="https://relay.example.com:9443"   # exactly control's SHIFT_RELAY_ENDPOINT
sudo shift-relay   --listen :9443   --cert /etc/shift-relay/relay-cert.pem   --key /etc/shift-relay/relay-key.pem   --client-ca /etc/shift/tls/machine-client-ca.pem
```

(`--database-url` and `--audience` accept the environment variables above
instead of flags.) The same command ships as a systemd unit —
`deployments/systemd/shift-relay.service` with
`relay.env.example` documenting every setting — running as the `shift`
system user; provision `/etc/shift/relay.env` (0600 root-owned) and the TLS
material as described in [Deployment](deployment.md#control-plane).

The relay's public certificate is ordinary server TLS. The ticket signer must
be provisioned separately in the control plane and relay secret stores. Do not
put it in a repository or command history.

## Ticket flow

The control plane issues a short-lived HMAC ticket bound to organization,
source machine, destination machine, migration ID, nonce, and expiry:

```http
POST /v1/organizations/ORG_ID/relay/tickets
Authorization: Bearer ...
```

Both agents then make an outbound `CONNECT /v1/relay/connect` request with the
ticket and their machine ID. The relay pairs exactly one source leg and one
destination leg, then forwards bytes in both directions. The peer mTLS
handshake occurs through that stream and remains the final machine identity
authority.

The current release ships the ticket/relay service primitives and tests, but
agent lifecycle enrollment and migration route selection are not enabled by
default yet. Direct public mTLS remains the supported remote migration path;
do not claim NAT traversal until the outbound agent session integration and
full two-agent relay migration test pass.

## Security boundaries

The relay can observe connection metadata, timing, and byte counts. It can
refuse or interrupt a transfer. It cannot decrypt checkpoint state, mint agent
identity, execute local agent commands, or substitute a destination without
the inner peer TLS and machine-ID checks succeeding.
