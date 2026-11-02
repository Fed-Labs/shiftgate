import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "CLI Reference",
  description: "Reference for the shiftgate commands implemented by this build with syntax, flags, and behavior.",
};

interface Command {
  name: string;
  syntax: string;
  description: string;
  flags?: string[];
}

interface CommandGroup {
  title: string;
  commands: Command[];
}

const COMMAND_GROUPS: CommandGroup[] = [
  {
    title: "General",
    commands: [
      {
        name: "doctor",
        syntax: "shiftgate doctor",
        description: "Verify the local Linux host, CRIU kernel support, GNU tar, state paths, and agent prerequisites. Prints one line per check and exits 3 when a required check fails.",
      },
      {
        name: "status",
        syntax: "shiftgate status [MIGRATION_ID]",
        description: "Show migration status: a table of the migrations this agent is running, or one migration's stage and event history when an ID is given.",
      },
      {
        name: "gc",
        syntax: "shiftgate gc [--grace DURATION] [--dry-run]",
        description: "Plan chunk garbage collection. Live sweeping is disabled by design — the command prints the exact offline shift-gc procedure (stop the agent, run shift-gc --state-dir … --grace … --dry-run, start the agent). --grace is the minimum age an orphaned chunk must reach before deletion (default 24h); --dry-run (default) plans only.",
        flags: ["--grace <duration>", "--dry-run"],
      },
      {
        name: "version",
        syntax: "shiftgate version",
        description: "Print the version of this shiftgate build.",
      },
      {
        name: "completion",
        syntax: "shiftgate completion bash|zsh|fish",
        description: "Emit a shell completion script for the given shell.",
      },
    ],
  },
  {
    title: "Machine management",
    commands: [
      {
        name: "machines",
        syntax: "shiftgate machines [list]",
        description: "Show this machine: id, OS, CPU, memory, GPU, and CRIU. With --control-url (or SHIFT_CONTROL_URL) set — including the platform endpoint — show the organization's fleet instead.",
      },
      {
        name: "machine",
        syntax: "shiftgate machine",
        description: "Alias of shiftgate machines, kept for the quickstart's identity check on a migration destination.",
      },
      {
        name: "machines register",
        syntax: "shiftgate machines register --machine-id ID --name NAME --agent-url URL",
        description: "Register a machine with the control plane so it can be a migration source or destination. Always talks to the control plane (the platform endpoint by default), so it needs shiftgate login first and never silently no-ops against the local agent.",
        flags: ["--machine-id <id>", "--name <name>", "--agent-url <url>"],
      },
      {
        name: "machines capability",
        syntax: "shiftgate machines capability NAME [--kind number|text|boolean] [--minimum N]",
        description: "Fleet query through the control plane's capability projection: which registered machines report a given scalar capability. --kind selects the comparison; --minimum applies only to numbers. Objects and arrays are not queryable.",
        flags: ["--kind number|text|boolean", "--minimum <value>"],
      },
      {
        name: "global option",
        syntax: "shiftgate --agent unix://PATH|https://HOST:PORT",
        description: "Select the local agent endpoint (default unix:///run/shift/agent.sock, or $SHIFT_AGENT_ENDPOINT). Remote endpoints require mTLS credentials.",
      },
      {
        name: "global option",
        syntax: "shiftgate --control-url URL COMMAND",
        description: "Point control-plane commands at a private deployment instead of the platform endpoint (or $SHIFT_CONTROL_URL, empty meaning the platform endpoint).",
      },
      {
        name: "global flags",
        syntax: "shiftgate [--json] [--timeout DURATION] [--token-store FILE] COMMAND",
        description: "Global flags are position-insensitive: shiftgate doctor --json and shiftgate --json doctor are the same command, and flags may appear anywhere before the '--' that starts a workload command line (never after it). --token-store overrides where the control-plane session is stored (default ~/.config/shift/cli-session.json). --timeout accepts plain seconds (30) or durations (30s, 15m); before the command word it is the request timeout, after a command it is that command's operation timeout — accepted only by checkpoint create, restore, migrate, fork, clone, workload stop, and standby trigger, defaulting to 30m, 30m, 2h, 30m, 1h, 10s (graceful stop), and 15m respectively.",
      },
    ],
  },
  {
    title: "Workload management",
    commands: [
      {
        name: "workloads",
        syntax: "shiftgate workloads",
        description: "Shorthand for 'shiftgate workload list' (fleet list when --control-url is set).",
      },
      {
        name: "workload create",
        syntax: "shiftgate workload create NAME --path PATH -- COMMAND [ARGS...]",
        description: "Register an explicitly scoped local workload with the connected agent; use --start to launch it immediately. --checkpoint-every arms a periodic checkpoint policy from the start.",
        flags: [
          "--path <dir>",
          "--workdir <dir>",
          "--start",
          "--memory <size>",
          "--cpu <count>",
          "--network preserve|reconnect|drain",
          "--device-policy reject_incompatible|warn_incompatible",
          "--env KEY=VALUE",
          "--gpu vendor=,model=,…",
          "--checkpoint-every <duration>",
          "--checkpoint-keep <n>",
        ],
      },
      {
        name: "workload inspect",
        syntax: "shiftgate workload inspect ID",
        description: "Show a workload spec, runtime state, process identity, and latest checkpoint reference. Like every inspect subcommand, always emits JSON.",
      },
      {
        name: "workload policy",
        syntax: "shiftgate workload policy NAME (--every 10m [--keep 5] | --off)",
        description: "Install or clear a periodic checkpoint policy: take a full checkpoint every interval while the workload runs, optionally retaining only the newest N. The interval floor is 10 seconds and the schedule is measured from the newest checkpoint. --off removes it (refused while a failover designation still waits on it).",
        flags: ["--every <duration>", "--keep <n>", "--off"],
      },
      {
        name: "workload failover",
        syntax: "shiftgate workload failover NAME (--to URL [--machine-id ID] [--keep N] | --off)",
        description: "Install a warm-standby failover policy: the newest root checkpoint is replicated to the standby agent at --to. --keep 0 (the default) retains every replica on the standby; --off withdraws the duty and leaves already-replicated checkpoints behind.",
        flags: ["--to <peer-url>", "--machine-id <id>", "--keep <n>", "--off"],
      },
      {
        name: "workload lifecycle",
        syntax: "shiftgate workload start|pause|resume ID",
        description: "Control a workload's process: launch it, stop the process group temporarily, or continue it.",
      },
      {
        name: "workload stop",
        syntax: "shiftgate workload stop ID [--timeout DURATION]",
        description: "Stop a running process group and release its runtime record after the grace period. --timeout accepts plain seconds or durations.",
        flags: ["--timeout <duration>"],
      },
      {
        name: "workload delete",
        syntax: "shiftgate workload delete ID [--yes]",
        description: "Remove a workload. Destructive: prompts on a terminal and refuses without --yes in scripts.",
        flags: ["--yes"],
      },
      {
        name: "workload logs",
        syntax: "shiftgate workload logs ID [--tail LINES]",
        description: "Show the workload's captured stdout/stderr — the trailing lines (default 200). Also available as shiftgate logs ID.",
        flags: ["--tail <lines>"],
      },
    ],
  },
  {
    title: "Checkpoint management",
    commands: [
      {
        name: "checkpoint create",
        syntax: "shiftgate checkpoint create WORKLOAD [--parent ID]",
        description: "Create a full checkpoint by default, or an incremental checkpoint when a retained parent is supplied. The workload keeps running by default (--leave-running); --stop-when-done is the explicit way to ask for the opposite.",
        flags: ["--parent <checkpoint-id>", "--leave-running", "--stop-when-done", "--tcp-state", "--timeout <duration>"],
      },
      {
        name: "checkpoint list",
        syntax: "shiftgate checkpoint list [--workload ID]",
        description: "List checkpoint summaries retained by the selected agent. KIND is full or incremental; SIZE is the pre-deduplication size and STORED is what the content-addressed store kept after deduplication.",
        flags: ["--workload <id>", "--json"],
      },
      {
        name: "checkpoint inspect",
        syntax: "shiftgate checkpoint inspect ID",
        description: "Show one checkpoint's manifest and accounting. Always emits JSON.",
      },
      {
        name: "restore",
        syntax: "shiftgate restore CHECKPOINT_ID [--lazy] [--timeout SECONDS]",
        description: "Restore locally through the selected agent, validate health, and commit only after validation succeeds. --lazy starts the process before its memory is fully loaded; pages stream in on demand via userfaultfd (needs criu check --feature uffd; refused up front with the reason when missing, never silently downgraded).",
        flags: ["--lazy", "--timeout <duration>"],
      },
      {
        name: "fork",
        syntax: "shiftgate fork WORKLOAD [--name NAME] [--root PATH] [--checkpoint ID] [--activate]",
        description: "Derive one independent workload from a workload's live state or one of its checkpoints, with its own re-encrypted full checkpoint and key namespace. The fork lineage is recorded; the source is untouched (a running source is checkpointed with leave-running).",
        flags: ["--name <name>", "--root <dir>", "--checkpoint <id>", "--activate", "--timeout <duration>", "list", "inspect FORK_ID"],
      },
      {
        name: "clone",
        syntax: "shiftgate clone CHECKPOINT_ID [--count N]",
        description: "Derive many independent running workloads from one checkpoint on this machine (count capped at 128). Members materialize in parallel and filesystem copies are reflink clone-on-write where the filesystem supports it. The set is all-or-nothing — any member failure rolls back every member. A workload that declares TCP ports clones one member only. A committed set is torn down through the workload API, never clone rollback (refused after commit; it exists for sets interrupted before it, and prompts — --yes in scripts).",
        flags: ["--count <1-128>", "--prefix <name>", "--parallel <1-16>", "--timeout <duration>", "list", "inspect CLONE_ID", "rollback CLONE_ID [--yes]"],
      },
      {
        name: "mirror",
        syntax: "shiftgate checkpoint mirror CHECKPOINT_ID",
        description: "Retry publication to configured local or S3-compatible object storage after a transient failure. Idempotent; verifies objects already present.",
      },
    ],
  },
  {
    title: "Migration",
    commands: [
      {
        name: "migrate",
        syntax: "shiftgate migrate WORKLOAD --to ALIAS|https://HOST:PORT [--machine-id ID]",
        description: "Migrate a running workload to a different machine. --to takes a saved alias (from connect) or an explicit HTTPS peer URL; an alias fills the destination URL, machine identity, and TLS server name from the saved profile. --mode cold (default) pauses, checkpoints, transfers, and restores; --mode live runs CRIU pre-dump passes while the workload keeps running and freezes it only for the final delta. --dry-run (alias --preflight) reports whether the migration would be admitted without moving anything: exit 0 means it would be admitted, exit 3 means it would be rejected.",
        flags: [
          "--mode cold|live",
          "--to <alias|peer-url>",
          "--server-name <tls-name>",
          "--machine-id <id>",
          "--pre-copy-passes <0-16>",
          "--dry-run",
          "--preflight",
          "--wait|--wait=false",
          "--timeout <duration>",
        ],
      },
      {
        name: "connect",
        syntax: "shiftgate connect ALIAS --to https://HOST:PORT --machine-id ID",
        description: "Save a remote machine alias for migrate and failover destinations. --server-name pins the TLS name when it differs from the URL host.",
        flags: ["--to <peer-url>", "--machine-id <id>", "--server-name <name>"],
      },
      {
        name: "connections",
        syntax: "shiftgate connections",
        description: "List saved remote machine aliases.",
      },
      {
        name: "disconnect",
        syntax: "shiftgate disconnect ALIAS",
        description: "Remove a saved remote machine alias.",
      },
      {
        name: "failover",
        syntax: "shiftgate failover [status]",
        description: "Show what this machine replicates to warm standbys: each policy's standby, the last checkpoint pushed, retention, and any replication error.",
      },
      {
        name: "standby",
        syntax: "shiftgate standby trigger WORKLOAD_ID [--checkpoint ID] [--lazy]",
        description: "The standby-side view: 'list' shows the duties this machine holds and whether automatic failover is armed; 'trigger' restores one now (keyed by workload id — names are not unique across sources). The trigger probes the source first and warns when it still answers; there is no fencing on the manual path, so an explicit trigger can run an independent copy if the source is actually alive.",
        flags: ["list", "trigger WORKLOAD_ID", "--checkpoint <id>", "--lazy", "--timeout <duration>"],
      },
    ],
  },
  {
    title: "Updates",
    commands: [
      {
        name: "update",
        syntax: "shiftgate update status|check|apply|rollback|block|unblock",
        description: "Manage this machine's agent updates through the local API: status shows running and pending versions, available and refused releases, preserved backups, blocked versions, and history; check consults the release feed immediately; apply installs the current selection or one named version; rollback reinstalls a preserved binary and blocks the version it undid; block/unblock manage the local block list. A release installs only after its signatures verify and the staged binary passes its --version self-check; an install that would interrupt a migration, restore, fork, or incoming transfer exits 6 to be retried later.",
        flags: ["status", "check", "apply [--version V]", "rollback [--backup ID] [--yes]", "block VERSION [--reason TEXT]", "unblock VERSION"],
      },
    ],
  },
  {
    title: "Authentication",
    commands: [
      {
        name: "login",
        syntax: "shiftgate login [--email ADDRESS] [--sso]",
        description: "Create a control-plane session and store it (single sign-on with --sso). The password is prompted on the terminal with echo disabled — never accepted as a flag. --sso authenticates through the control plane's OIDC issuer with a loopback redirect and PKCE; it is also the only login for domains whose organizations enforce single sign-on.",
        flags: ["--email <address>", "--sso"],
      },
      {
        name: "logout",
        syntax: "shiftgate logout",
        description: "Revoke the stored control-plane session and clear the stored tokens.",
      },
      {
        name: "whoami",
        syntax: "shiftgate whoami",
        description: "Show the signed-in identity and organizations.",
      },
      {
        name: "output",
        syntax: "shiftgate --json COMMAND",
        description: "Emit machine-readable JSON (like every global flag, accepted before or after the command word). 'inspect' subcommands always emit JSON regardless.",
      },
    ],
  },
  {
    title: "Control plane",
    commands: [
      {
        name: "plans",
        syntax: "shiftgate plans",
        description: "Show the public plan catalog: price, machine and storage limits, and features per tier — the same catalog the control plane enforces entitlements from.",
      },
      {
        name: "storage",
        syntax: "shiftgate storage [--org ID]",
        description: "Show the organization's hosted checkpoint storage status: reconciled usage against the plan limit, the bucket location, and — when hosting is enabled — the exact environment lines to put on an agent for hosted mirroring. The agent fetches its own short-lived credentials, so no storage secret is printed.",
        flags: ["--org <id>"],
      },
      {
        name: "fleet",
        syntax: "shiftgate fleet [--org ID] machines|workloads|migrations|checkpoints|audit|retention|sso|api-keys|entitlement|usage",
        description: "Manage the organization's registry. 'migrations [ID]' lists jobs or shows one with its event stream; 'checkpoints [WORKLOAD]' lists fleet checkpoints; 'retention' shows or sets the retention windows in days; 'sso' manages domain enforcement; 'entitlement' and 'usage [--from YYYY-MM-DD --to YYYY-MM-DD]' report plan entitlements and metered usage. The organization defaults to the first the account belongs to; --org selects another.",
        flags: ["--org <id>"],
      },
      {
        name: "fleet api-keys",
        syntax: "shiftgate fleet api-keys [create --name N [--scopes S] | revoke KEY --yes]",
        description: "List, create, or revoke organization API keys. The create response shows the secret exactly once — only a digest is stored. Revoke is destructive: prompts on a terminal and refuses without --yes in scripts.",
        flags: ["create --name <name>", "--scopes <s>", "revoke KEY", "--yes"],
      },
      {
        name: "fleet retention",
        syntax: "shiftgate fleet retention [--audit-days N] [--checkpoint-days N] [--deleted-storage-days N]",
        description: "Show or set the organization's retention windows in days. An omitted flag leaves its window unchanged; 0 is the deliberate keep-forever policy. Plan-capped: Free and Pro up to 365 days, Business and Enterprise up to 36500.",
        flags: ["--audit-days <n>", "--checkpoint-days <n>", "--deleted-storage-days <n>"],
      },
      {
        name: "fleet sso",
        syntax: "shiftgate fleet sso [--enforce DOMAIN | --disable]",
        description: "Manage single sign-on enforcement (admin role; the control plane must have an OIDC issuer configured). Enforcing claims an email domain: its accounts then authenticate with login --sso and password login is refused; disabling releases the claim.",
        flags: ["--enforce <domain>", "--disable"],
      },
      {
        name: "marketplace",
        syntax: "shiftgate marketplace [--org ID] offers|inventory|place|publish|withdraw|reservations|reserve|commit|release|fail",
        description: "Trade compute on the marketplace (alias: shiftgate market). 'inventory' shows everything schedulable now; 'offers' this organization's offers; 'publish' offers spare capacity (--machine --cpu --memory [--region --country --visibility]); 'place' ranks destinations without holding anything; 'reserve' holds capacity server-side (it expires or is released rather than pinning capacity forever); 'commit | release | fail --reservation ID [--reason TEXT]' settles it. 'withdraw --offer ID' is destructive: prompts on a terminal and refuses without --yes in scripts.",
        flags: ["--org <id>"],
      },
    ],
  },
];

const EXIT_CODES = [
  ["1", "Unexpected or API errors"],
  ["2", "Usage errors"],
  ["3", "Failed prerequisites or transport (including a refused preflight and a missing offline GC)"],
  ["4", "Missing resources"],
  ["5", "Failed or rolled-back migrations"],
  ["6", "Deferred updates (an install that would interrupt an in-flight operation)"],
];

export default function CliPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Reference
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            CLI reference
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            Every implemented <code className="font-mono text-sm bg-bg-surface px-1.5 py-0.5 rounded">shiftgate</code> command
            with syntax, flags, and descriptions. Global flags (
            <code className="font-mono text-sm bg-bg-surface px-1.5 py-0.5 rounded">--agent</code>,{" "}
            <code className="font-mono text-sm bg-bg-surface px-1.5 py-0.5 rounded">--control-url</code>,{" "}
            <code className="font-mono text-sm bg-bg-surface px-1.5 py-0.5 rounded">--token-store</code>,{" "}
            <code className="font-mono text-sm bg-bg-surface px-1.5 py-0.5 rounded">--json</code>,{" "}
            <code className="font-mono text-sm bg-bg-surface px-1.5 py-0.5 rounded">--timeout</code>)
            work anywhere before the <code className="font-mono text-sm bg-bg-surface px-1.5 py-0.5 rounded">--</code>{" "}
            that starts a workload command line — order-insensitive, before or
            after the command word.
          </p>
        </div>

        <div className="space-y-12">
          {COMMAND_GROUPS.map((group) => (
            <div key={group.title}>
              <h2 className="text-lg font-semibold mb-6 pb-2 border-b border-border-subtle">
                {group.title}
              </h2>

              <div className="space-y-6">
                {group.commands.map((cmd) => (
                  <div key={cmd.name} className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
                    <div className="p-5">
                      <div className="flex items-start justify-between gap-4 mb-3 flex-wrap">
                        <h3 className="text-sm font-semibold font-mono text-text">
                          {cmd.syntax}
                        </h3>
                      </div>
                      <p className="text-sm text-text-secondary leading-relaxed mb-3">
                        {cmd.description}
                      </p>
                      {cmd.flags && cmd.flags.length > 0 && (
                        <div>
                          <p className="text-xs font-mono text-text-muted uppercase tracking-wider mb-2">
                            Flags
                          </p>
                          <div className="flex flex-wrap gap-2">
                            {cmd.flags.map((flag) => (
                              <code
                                key={flag}
                                className="text-xs font-mono bg-bg-surface border border-border-subtle rounded px-2 py-1 text-text-secondary"
                              >
                                {flag}
                              </code>
                            ))}
                          </div>
                        </div>
                      )}
                    </div>
                  </div>
                ))}
              </div>
            </div>
          ))}
        </div>

        {/* Exit codes */}
        <div className="mt-16">
          <h2 className="text-lg font-semibold mb-6 pb-2 border-b border-border-subtle">
            Exit codes
          </h2>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <table className="w-full text-sm">
              <tbody className="divide-y divide-border-subtle">
                {EXIT_CODES.map(([code, desc]) => (
                  <tr key={code}>
                    <td className="px-5 py-3 font-mono text-xs text-accent w-16">{code}</td>
                    <td className="px-5 py-3 text-xs text-text-secondary">{desc}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>

        {/* Destructive commands */}
        <div className="mt-12 rounded-lg border border-status-warning/20 bg-status-warning/5 p-6">
          <h2 className="text-sm font-semibold text-status-warning mb-3">
            Destructive commands confirm twice
          </h2>
          <p className="text-sm text-text-secondary leading-relaxed">
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">workload delete</code>,{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">clone rollback</code>,{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">update rollback</code>,{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">fleet api-keys revoke</code>,
            and{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">marketplace withdraw</code>{" "}
            prompt on a terminal and refuse without{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">--yes</code>{" "}
            in scripts.
          </p>
        </div>
      </Container>
    </Section>
  );
}
