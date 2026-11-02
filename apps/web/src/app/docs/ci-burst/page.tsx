import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";
import { ArrowRight } from "lucide-react";

export const metadata: Metadata = {
  title: "CI-burst proof kit",
  description:
    "One warm checkpoint, N workers, one move, one proven rollback. Reproducible CI burst on hardware you own — every step prints its measured duration.",
};

const STEPS = [
  {
    number: "01",
    title: "Install and check the machine",
    description:
      "One command on an x86_64 Linux host: it downloads the release, verifies its SHA-256, installs the binaries, installs CRIU through your package manager when it is missing, and enables the agent as a systemd service with a local-only configuration. Then doctor verifies CRIU health, kernel features, archive tooling, and privileges — real checks, no fake checkpoints (exit 3 when a required check fails).",
    command: `curl -fsSL https://github.com/Fed-Labs/shiftgate/releases/latest/download/install.sh | bash
shiftgate doctor`,
    output: `OK    platform linux/amd64
OK    privileges agent effective uid 0
OK    criu 4.2
OK    cgroup_v2 mounted, delegation available
OK    archive GNU tar available`,
  },
  {
    number: "02",
    title: "Create a runner-like workload",
    description:
      "A warmed CI worker is a long-lived process with hot caches and a fetched dependency tree. The proof uses a stateful writer with the benchmark methodology (about a 100 MiB heap, one line per second). Only the approved workload root travels with the checkpoint; open files outside the root are resolved and reported, never silently captured.",
    command: `mkdir -p ~/ci-demo
shiftgate workload create runner --path ~/ci-demo --start -- python3 ~/ci-demo/writer.py`,
    output: `Workload wl_runner created.
Status: running
Root:   /home/you/ci-demo`,
  },
  {
    number: "03",
    title: "Checkpoint the warm state once",
    description:
      "The capture is encrypted, content-addressed, and immutable. --leave-running is the default: the process resumes automatically after the dump. On the bench box a full checkpoint took 914 to 973 ms (docs/benchmarks.md) — the script prints the measured duration for your machine.",
    command: `shiftgate checkpoint create runner --leave-running
shiftgate checkpoint list --workload runner`,
    output: `Checkpoint ckpt_example created. ... -> ..., deduplicated ..., duration 959ms.
ID            WORKLOAD  KIND  CREATED              SIZE    STORED
ckpt_example  runner    full  2026-10-19 08:00:00  17.4MB  ...`,
  },
  {
    number: "04",
    title: "Clone it into N workers — the burst",
    description:
      "One warm checkpoint becomes N independent running workloads, each with its own root and identity, reflink-copied where the filesystem allows. No re-warm per worker: this is CI without cold starts on hardware you own. Clone is same-machine — moving across machines is a migrate, not a clone.",
    command: `shiftgate clone CHECKPOINT_ID --count 10 --prefix ci
shiftgate clone list`,
    output: `Cloned 10 workloads from ckpt_example as ci-1 … ci-10.`,
  },
  {
    number: "05",
    title: "Move the workload to a second machine",
    description:
      "A second host with its own install and a passing doctor is a migration target. Save it under an alias (or pass the peer URL directly). --preflight is a dry run: admitted or not, nothing moves. Cold moves are on the free tier; --mode live runs CRIU pre-dump passes while the workload keeps running and freezes it only for the final delta — a paid plan feature on every path, including peer-to-peer.",
    command: `shiftgate connect ci-2 --to https://second-host:8443 --machine-id MACHINE_ID
shiftgate migrate runner --to ci-2 --preflight
shiftgate migrate runner --to ci-2
shiftgate status MIGRATION_ID`,
    output: `Preflight: destination compatible. Nothing was moved.
Migration mig_example created.
CREATED → DISCOVER → VALIDATE → SNAPSHOT → PREPARE → TRANSFER
VERIFY → RESTORE → POST_VALIDATE → SWITCH → COMMIT → COMPLETED
downtime (freeze → health-validated restore) is reported in the migration record.`,
  },
  {
    number: "06",
    title: "Fail a move on purpose — the source survives",
    description:
      "The safety half of the proof. Point a migration at an unreachable peer: the persisted state machine refuses to commit, the move rolls back, and the source workload keeps running. Nothing at any stage destroys the source — that is the invariant the e2e suite enforces by failing moves on purpose. The approved caption: the move failed. The source is still running.",
    command: `shiftgate migrate runner --to https://127.0.0.1:1
shiftgate workload inspect runner`,
    output: `Migration failed at VALIDATE: destination unreachable. Rolled back; source preserved.
Workload wl_runner — status: running.`,
  },
];

const LIMITS = [
  "Linux x86_64; a privileged agent and a CRIU-capable kernel are required (criu check must pass).",
  "A live move is a measured freeze reported as downtime (3.2–3.5 s live vs 3.7–5.5 s cold on the benchmark workloads; the transfer inside the freeze was 107–223 ms on loopback).",
  "Clone is same-machine. Cross-machine is a migrate.",
  "Live mode is a paid plan feature on every path, including peer-to-peer. The free tier is cold-only (2 machines, 10 GB) and always includes checkpoints, restores, same-machine clones, and dry-run/preflight.",
  "TCP socket state needs CRIU TCP repair where the kernel permits it; unsupported state is rejected, not faked.",
  "Pre-1.0: a MINOR bump may break automation — see the versioning promise.",
];

export default function CiBurstPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Proof kit
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            CI without cold starts, on hardware you own
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            One reproducible path: prepare a warm runner-like workload, checkpoint it
            once, clone it into N workers, move it to a second machine, and deliberately
            fail a move to show that the source survives. Every step prints its measured
            duration. State moves — and a failed move never destroys the source.
          </p>
        </div>

        <div className="space-y-12">
          {STEPS.map((step, i) => (
            <div key={step.number} className="relative">
              {i < STEPS.length - 1 && (
                <div className="absolute left-[18px] top-12 bottom-[-3rem] w-px bg-border-subtle hidden md:block" />
              )}

              <div className="flex gap-5">
                <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-bg-surface border border-border text-sm font-mono font-semibold text-accent">
                  {step.number}
                </div>

                <div className="min-w-0 flex-1">
                  <h2 className="text-xl font-semibold mb-2">{step.title}</h2>
                  <p className="text-text-secondary text-sm leading-relaxed mb-4">
                    {step.description}
                  </p>

                  <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
                    <div className="flex items-center gap-2 border-b border-border-subtle px-4 py-2">
                      <div className="h-2 w-2 rounded-full bg-status-error/60" />
                      <div className="h-2 w-2 rounded-full bg-status-warning/60" />
                      <div className="h-2 w-2 rounded-full bg-status-healthy/60" />
                      <span className="ml-2 text-xs text-text-muted font-mono">terminal</span>
                    </div>
                    <div className="p-4">
                      <pre className="text-sm text-accent">
                        <code>{step.command}</code>
                      </pre>
                      {step.output && (
                        <>
                          <div className="my-3 h-px bg-border-subtle" />
                          <pre className="text-sm text-text-secondary whitespace-pre-wrap">
                            <code>{step.output}</code>
                          </pre>
                        </>
                      )}
                    </div>
                  </div>
                </div>
              </div>
            </div>
          ))}
        </div>

        <div className="mt-16 rounded-lg border border-border bg-bg-elevated p-6">
          <h3 className="text-sm font-semibold mb-3">Run it unattended</h3>
          <p className="text-sm text-text-secondary mb-4">
            The same path as a script — it prints a per-step metrics table and exits 0 when
            N workers are running from one checkpoint and the forced-failure move rolled back
            with the source alive. Set DEST_URL to include the cross-machine move.
          </p>
          <pre className="text-sm text-accent">
            <code>CLONE_COUNT=10 ./scripts/demo-ci-burst.sh</code>
          </pre>
        </div>

        <div className="mt-10 rounded-lg border border-status-warning/40 bg-bg-elevated p-6">
          <h3 className="text-sm font-semibold mb-3">Honest limits (read these)</h3>
          <ul className="space-y-2 text-sm text-text-secondary list-disc pl-5">
            {LIMITS.map((line) => (
              <li key={line}>{line}</li>
            ))}
          </ul>
          <p className="text-sm text-text-secondary mt-4">
            Every number quoted here comes from{" "}
            <a href="/docs/benchmarks" className="text-text hover:text-accent transition-colors">
              docs/benchmarks.md
            </a>{" "}
            and its documented harness — rerun it on your hardware before planning around it.
            Full boundary list:{" "}
            <a href="/docs/troubleshooting" className="text-text hover:text-accent transition-colors">
              limitations and troubleshooting
            </a>
            .
          </p>
        </div>

        <div className="mt-16 rounded-lg border border-border bg-bg-elevated p-6">
          <h3 className="text-sm font-semibold mb-3">Next steps</h3>
          <ul className="space-y-2 text-sm text-text-secondary">
            <li className="flex items-center gap-2">
              <ArrowRight size={14} className="text-accent shrink-0" />
              <span>
                Read the{" "}
                <a href="/docs/quickstart" className="text-text hover:text-accent transition-colors">
                  quickstart
                </a>{" "}
                for policies, lazy restore, and standby/failover
              </span>
            </li>
            <li className="flex items-center gap-2">
              <ArrowRight size={14} className="text-accent shrink-0" />
              <span>
                Clone one checkpoint into N workers in depth:{" "}
                <a href="/guides/clone-checkpoint-into-n-workers" className="text-text hover:text-accent transition-colors">
                  the clone guide
                </a>
              </span>
            </li>
            <li className="flex items-center gap-2">
              <ArrowRight size={14} className="text-accent shrink-0" />
              <span>
                Plan a maintenance window:{" "}
                <a href="/guides/linux-maintenance-window-without-downtime-cost" className="text-text hover:text-accent transition-colors">
                  the maintenance window playbook
                </a>
              </span>
            </li>
            <li className="flex items-center gap-2">
              <ArrowRight size={14} className="text-accent shrink-0" />
              <span>
                Every command and flag used here is in the{" "}
                <a href="/docs/cli" className="text-text hover:text-accent transition-colors">
                  CLI reference
                </a>
              </span>
            </li>
          </ul>
        </div>
      </Container>
    </Section>
  );
}
