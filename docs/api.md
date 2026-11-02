# API Contract

The versioned control-plane contract is [OpenAPI](../api/openapi/shift.yaml). It covers
authentication (password and single sign-on), organizations, machines, workload metadata,
checkpoints, migration jobs, migration events, API keys, usage, entitlements, the plan
catalog, checkout and billing portal, audit events, retention policies, hosted checkpoint
storage (status, agent credential issuance, usage reconciliation), health, Stripe
webhooks, the agent command channel (machine-scoped poll/complete), and SCIM v2 user
provisioning. Also live and typed in the spec: trials (`GET`/`POST
/v1/organizations/{id}/trial`, `TrialStatus`/`TrialStarted`), referrals (`GET
/v1/organizations/{id}/referral`, `POST /v1/organizations/{id}/referral/claim`,
`ReferralStats`), usage rollups (`GET /v1/organizations/{id}/usage/summary`,
`UsageSummaryReport`), public platform numbers (`GET /v1/public/stats`,
`PublicStats`), and the admin scoreboard (`GET /v1/admin/scoreboard`,
`ScoreboardCounters` — platform-admin only).

API keys are opaque `Bearer` credentials. They are bound to the organization in which they
were created and can carry broad (`read`, `operate`, `admin`) or resource-specific
(`machines`, `workloads`, `migrations`, `checkpoints`, `compute`, `scim`) scopes. The
create response contains the secret exactly once; only an HMAC digest is persisted, so a
lost secret must be revoked and replaced. API-key metadata never contains the secret.

Checkpoint registration stores manifest and accounting metadata in PostgreSQL; encrypted
content-addressed chunks remain on agents. Migration events are append-only and returned in
sequence order. Usage queries accept inclusive `from` and exclusive `to` ISO-8601 dates.
Control-plane list endpoints paginate with `limit` (1–500, default 100; values above the
cap are clamped) and `offset` (records to skip) over a stable newest-first order — the
audit-event list included (limit clamped at 500, `offset` walks deeper into the trail;
there is no cursor or date-range seek — see [limitations.md](limitations.md)).

The local agent contract is intentionally separate because it uses Unix peer credentials
and machine-to-machine TLS. Its endpoints are rooted at `/v1/health`, `/v1/machine`,
`/v1/workloads`, `/v1/checkpoints`, `/v1/restores`, `/v1/forks`, `/v1/clones`,
`/v1/migrations`, `/v1/standby`, `/v1/failover`, and `/v1/updates`.

`POST /v1/workloads/{id}/fork` and `POST /v1/checkpoints/{id}/fork` derive an independent
workload from a workload's live state or from one explicit checkpoint. Both accept `name`,
`root_path`, `checkpoint_id`, `activate`, and `timeout_seconds`, and return the fork
transaction record, including the fork's new workload id, its own full checkpoint id, and
its root path. Forking a running workload checkpoints it with `leave_running`, so the
source is never interrupted. `GET /v1/forks` and `GET /v1/forks/{id}` return fork records,
filtered by the caller's ownership of the source workload.

`POST /v1/checkpoints/{id}/clone` derives a set of independent running workloads from one
checkpoint on the same machine. The request takes `count` (1–128), `name_prefix`,
`parallel` (concurrent restores, default 4, capped at 16), and `timeout_seconds`, and
returns the clone-set record: per-member workload id, root path, pid, and outcome, plus
the set's all-or-nothing state. Members read the checkpoint's stored chunks once and
take reflink copies of the extracted image set, so a set costs N restores, not N full
copies. A workload that declares TCP ports clones one member only — every member would
rebind the same host port — and larger sets are refused up front with
`422 CLONE_FAILED`. `GET /v1/clones` and `GET /v1/clones/{id}` return clone-set records
(filtered by the caller's ownership of the source workload), and
`POST /v1/clones/{id}/rollback` reverts a set that was interrupted before commit; a
committed set is a fleet of ordinary workloads and its rollback is refused — teardown
goes through the workload API.

`POST /v1/checkpoints/{id}/restore` accepts a `lazy` flag: the restored process starts
executing before its memory image is fully loaded, and a `criu lazy-pages` daemon serves
page faults on demand while streaming the remainder in the background. It requires
kernel and CRIU userfaultfd support and is refused with the reason when either is
missing — never silently downgraded to an eager restore. Every restore record reports
`time_to_first_execution_ms` so eager and lazy compare honestly. `GET /v1/restores` and
`GET /v1/restores/{id}` return restore records.

`POST /v1/workloads/{id}/failover` installs, replaces, or clears the workload's warm
standby designation: `agent_url` names the standby's peer listener, `machine_id` pins
its identity, and `keep_last` bounds what the standby retains. `off: true` withdraws
the designation; already-replicated checkpoints stay on the standby. `GET /v1/failover` returns the source-side replication ledger, and on
the standby machine `GET /v1/standby` returns the duties it holds with their armed
checkpoints and failover history, while `POST /v1/standby/{workload}/trigger` restores
a held duty here — with the same optional `lazy` trade-off — after probing the source
and warning when it still answers.

`POST /v1/checkpoints/{id}/mirror` retries publication of a checkpoint already committed
to the agent's encrypted local repository. The operation is idempotent and does not alter
the workload runtime state.

`POST /v1/workloads/{id}/policy` arms or changes a periodic checkpoint policy on the
workload: `interval_seconds` (floor 10) and optional `keep_last` retention count. A
missing or non-positive interval removes the policy. While the workload runs, the agent
takes a full `leave_running` checkpoint every interval and, with `keep_last`, prunes the
oldest checkpoints beyond the count — never an ancestor a kept checkpoint still needs.
The schedule is measured from the newest checkpoint, so it survives agent restarts, and
a checkpoint already in flight for the workload makes a concurrent create fail with
`422 CHECKPOINT_FAILED` rather than queue. The response is the updated workload; an
impossible policy is `422 POLICY_INVALID`.

`POST /v1/migrations/preflight` runs a migration's discovery and validation without
creating one. The request is the same shape as migration creation; the agent reaches the
destination over the peer channel, inspects the source, runs the same compatibility
check the migration's validate stage would run on the same inputs, and returns both
machine profiles, the resolved destination identity, the compatibility report, and the
network plan the migration would apply. Nothing is frozen, moved, or recorded. An
unreachable destination is `502 DESTINATION_UNREACHABLE`; a destination that resolves to
the source machine or to a machine other than the requested id is `422
DESTINATION_IS_SOURCE` / `422 DESTINATION_IDENTITY_MISMATCH` — the same codes a real
migration would fail with.

## Migration jobs (control plane)

`POST /v1/organizations/{id}/migrations` registers a platform-mediated migration job:
`workload_id`, `source_machine_id`, `destination_machine_id`, optional `mode`
(`cold` (default) or `live`), optional `route` (`direct` or `relay`), an optional
`idempotency_key` (the `Idempotency-Key` header is preferred), and an optional
`timeout_seconds`. The job record returned (and returned by the job list/get endpoints)
carries `route`, and for relay-routed transfers `relay_endpoint` (the relay bridge
endpoint) and `relay_ticket` (a one-use ticket for the transfer session — never logged).
Peer-to-peer migrations between an operator's own agents never create a job here.

## Agent command channel

The control plane delivers work to registered agents over a machine-authenticated
channel. `POST /v1/organizations/{id}/machines/{machineID}/commands` (operator) queues a
command for the machine's agent; `GET /v1/organizations/{id}/machines/{machineID}/commands/poll`
lets the agent claim the next queued command (an empty queue is `204`), and
`POST /v1/organizations/{id}/machines/{machineID}/commands/{commandID}/complete` settles
it with the agent's outcome. Poll and complete require a machine-scoped API key bound to
the same organization — user sessions cannot claim or settle commands — so a stolen
browser session can never impersonate an agent. Delivery is at-most-once per claim: a
command's delivery window expires, an expired command is never claimed, and a
completion is accepted only from the machine and claim that polled it.

## Workload ownership and fenced failover

`POST /v1/organizations/{id}/ownership/acquire` returns the next monotone ownership
`epoch` for a workload on a machine, plus an opaque `token` that is returned exactly
once and never persisted in plaintext; a second, unexpired owner holding the workload is
`409`. `POST /v1/organizations/{id}/ownership/fence` confirms a fenced failover: the body
is `{workload_id, machine_id, epoch, token}` — the epoch and token from `acquire` — and
a successful response is `200 {state: "fenced", epoch}`. A stale epoch or a wrong token
is an ownership conflict (`409`). This is the fencing path automatic standby restore
requires (see [threat-model.md](threat-model.md)); lease expiry or source silence alone
never substitutes for it.

## Account erasure

`DELETE /v1/users/me` is the account-erasure mechanism: it irreversibly anonymizes the
calling user's personal data (email, display name, password hash, session tokens, and
audit IP/user-agent metadata) and revokes every session. The user row itself survives as
a pseudonymous stub so audit trails, organization membership, and billing history keep
valid references. The response is an erasure report of counts only —
`{user_id, email_anonymized, sessions_removed, api_keys_removed, audit_ips_scrubbed}` —
never the destroyed data. Accounts deactivated through SCIM reach the same end state through the
retention sweep's anonymization pass after a 30-day grace window (see
[limitations.md](limitations.md)).

## Fleet capability queries and retention

`GET /v1/organizations/{id}/machines/capability?capability=NAME&kind=KIND&minimum=N`
(viewer) answers a fleet query through the relational projection the control plane
maintains for every machine's JSONB capability document. `kind` is required —
`number`, `text`, or `boolean` — because the same capability name can hold different
types in different documents; `minimum` is valid only with `kind=number` and filters
machines below the floor. Results are full machine records, numbers ordered by value
descending. Only scalar leaves are projected; objects and arrays stay in the document.
The projection is rebuilt inside the same transaction as each machine write
(registration and heartbeat), so it can never disagree with the document at rest.

`GET /v1/organizations/{id}/retention` (admin) returns the organization's retention
windows in days. `PUT` with the same path accepts any subset of
`audit_retention_days`, `checkpoint_retention_days`, and
`deleted_storage_retention_days`; omitted fields keep their value, `0` is the
deliberate keep-forever policy (every tier), and values must be 0–36500 — but
are plan-capped: Free and Pro windows above 365 days are refused with
`402 RETENTION_PLAN_LIMIT` (Business/Enterprise may use the full range;
see [operations.md](operations.md#retention)). Changes are audited as
`retention.update` with the resulting windows in the metadata.

The control plane enforces the windows on a background sweep
(`retention_sweep_interval`, default hourly, zero disables): audit rows past their
window are deleted; checkpoints past theirs are marked `deleted` — the row and its
lineage survive, only restorability is dropped — and storage objects already
soft-deleted are purged after their grace window.

## Billing

`GET /v1/plans` is public and returns the plan catalog: each tier's name, price in cents
(`0` free, `-1` custom), seat model, description, advertised features, and the machine and
storage limits the control plane enforces. Each plan also carries `live_migration` —
false on the free tier — which the product enforces at every admission point: a live-mode
`POST /v1/organizations/{id}/migrations`, a live-mode `migrate` agent-command dispatch,
and a peer-to-peer live move driven through the CLI or the agent's local API are all
refused for a cold-only plan with `402 LIVE_MIGRATION_PLAN_REQUIRED` (the CLI reports the
same code as a failed command). Cold mode is never gated. The catalog is the single
source of plan truth — the pricing page renders it and entitlement checks use it.

## Organization members and seats

`POST /v1/organizations/{id}/members` (admin) upserts one membership by email with a
role (owner cannot be assigned by this endpoint; the owner role is immutable through
it). On per-seat plans the purchased seat quantity is the ceiling: an add past it is
`402 SEATS_EXCEEDED`, and a role change to an existing member never consumes a seat.
Member listing and removal are not part of the API today, and the entitlement
response does not carry the purchased seat count — the dashboard shows the
caller's own membership, and additions go through this endpoint; see
[limitations.md](limitations.md).

`POST /v1/organizations/{id}/billing/checkout` (admin) opens a Stripe-hosted checkout
session for a catalog plan and returns `{url, session_id, plan}`. The request supplies the
plan key plus absolute `success_url` and `cancel_url` (and a seat count for per-seat
plans); the session carries only the organization id and plan key as metadata. Plans
without a self-service price — free and enterprise — are rejected, as is any plan whose
Stripe price is not configured (`stripe_prices` in the control-plane configuration, or
`SHIFT_STRIPE_PRICE_<PLAN>`). When Stripe billing is not configured the route answers
`503 BILLING_NOT_CONFIGURED` rather than simulating a checkout.

`POST /v1/organizations/{id}/billing/portal` (admin) opens a Stripe billing-portal session
for the organization's stored customer and returns `{url}`. An organization that has never
subscribed has no Stripe customer and receives `400 PORTAL_NO_CUSTOMER`.

Subscription changes arrive as signed webhooks at `POST /v1/webhooks/stripe`. For a plan in
the catalog the webhook applies the catalog's limits — never the numbers the subscription
metadata claims — so the enforced entitlements always match the advertised plan. Plans
outside the catalog (negotiated tiers configured in Stripe) may carry their own
`max_storage_bytes` and `max_machines` metadata, with safe defaults when absent.

## Updates

`GET /v1/updates` reports the machine's update state for the agent component: the running
version, the channel and policy, a release staged on disk and pending a service restart,
the newest applicable release from the last check, every evaluated release with the reason
each was refused, preserved backups, blocked versions, and the recent history. Any
authenticated local caller may read it.

`POST /v1/updates/check` consults the configured release feed immediately and returns every
release evaluated for this machine, applicable or not, with the skip code and reason for
each refusal. `POST /v1/updates/apply` installs a release — the current selection when the
optional `version` is empty, or one specific version — after verifying signatures against
the configured key ring and running the staged binary's `--version` self-check.
`POST /v1/updates/rollback` reinstalls a preserved binary, choosing the most recent backup
or the one named in the optional `backup_id`, and blocks the version it undid so the
automatic loop cannot immediately reinstall it. `POST /v1/updates/block` (with `version`
and an optional `reason`) and `POST /v1/updates/unblock` manage the local block list.

The mutating endpoints are limited to root and the user the agent itself runs as, because
replacing the agent binary is equivalent to that power. An apply that arrives while a
migration, restore, fork, or incoming transfer is in flight returns `409 UPDATE_DEFERRED`
and is retried on the next check; the gate is consulted both before the download and again
immediately before the binary swap.

Clients should treat `4xx` responses as actionable input or authorization errors and
`5xx` responses as retryable only when the operation is idempotent. Every response carries
an `X-Request-ID` from the observability middleware.

## Single sign-on (OIDC)

When the control plane is configured with an OIDC issuer, organizations can claim an email
domain and require single sign-on for it. The flow is authorization code with PKCE S256
and a loopback redirect — RFC 8252's native-application pattern — so authorization codes
never travel to a remote host.

1. The client generates a PKCE verifier and challenge and starts a loopback listener.
2. `POST /v1/auth/sso/authorize` with `email`, `redirect_uri` (loopback http only), and
   `code_challenge` returns the issuer's `authorization_url` plus a `state`.
3. The browser completes authentication at the issuer, which redirects to the loopback
   listener with a one-time `code` and the same `state`.
4. `POST /v1/auth/sso/token` with `code`, `state`, `code_verifier`, and the same
   `redirect_uri` redeems the code. The control plane verifies the ID token's signature
   against the issuer's JWKS (RS256 and ES256), checks issuer, audience, expiry, and
   nonce, requires a verified email claim matching the one that started the flow, then
   links or provisions the local account and answers with session tokens.

The `state` is an HMAC-signed blob carrying the email, the nonce, and a ten-minute expiry —
no server-side session state, so any replica can finish the flow. Federated accounts are
created with an empty password hash, which no password can verify.

`GET /v1/auth/sso/required?email=...` tells clients whether a domain is under enforcement
before a password is attempted. While enforcement is on, `POST /v1/auth/login` and
`POST /v1/auth/register` refuse the domain with `403 SSO_REQUIRED`.

`GET|PUT /v1/organizations/{id}/sso` (admin) reads and changes enforcement. Enforcing SSO
claims one email domain per organization; a domain can be claimed by only one organization.
Disabling clears the claim — re-enabling is a deliberate, audited act. A federated login
automatically joins every enforcing organization whose domain matches the email as a
viewer.

## SCIM v2 provisioning

An identity provider provisions accounts through `/v1/scim/v2/Users`, authenticated by an
API key carrying the `scim` scope. The tenant is the key's own organization. Session
tokens cannot provision — not even an administrator's — so a compromised browser session
never reaches the directory.

The surface follows RFC 7644: `ServiceProviderConfig`, `ResourceTypes`, and `Schemas`
discovery endpoints; `GET /Users` with `filter`, `startIndex`, and `count` returning the
ListResponse shape; `POST /Users` create; `GET/PUT/PATCH/DELETE /Users/{id}`. Responses
use the `application/scim+json` content type and the protocol's error shape. `userName`
is the account's email — one address per account.

Filters support the operators `eq`, `co`, `sw`, `ew`, and `pr` over `userName`,
`emails.value`, `displayName`, `externalId`, and `active`, joined by `and`. Attribute and
operator names come from fixed whitelists; values bind as query parameters only. Anything
else — `or`, `not`, parentheses, attributes this store does not expose — is a `400`,
refused rather than mis-executed. `PATCH` supports only the `replace` operation on
`active`, `displayName`, `externalId`, and `userName`; unsupported operations and paths
are `400`s, never silently dropped.

SCIM delete means deactivation: the account's live sessions are revoked in the same
transaction, and the row survives so audit trails keep pointing at a real account. The
deactivated account's personal data does not survive forever — see
[Account erasure](#account-erasure) for the grace-window sweep and `DELETE
/v1/users/me`. Every
provisioning mutation is audited (`scim.user.create`, `scim.user.replace`,
`scim.user.patch`, `scim.user.delete`). Only the Users resource is implemented; Groups is
not — see [limitations](limitations.md).
