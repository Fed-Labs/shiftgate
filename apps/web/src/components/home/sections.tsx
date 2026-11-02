"use client";

import Link from "next/link";
import { Reveal } from "@/components/motion/liquid";
import { TerminalFlow, type TermLine } from "@/components/home/terminal";

const MOVES: Array<[string, string]> = [
  ["PROCESS", "pid trees, threads, signals, timers"],
  ["MEMORY", "pages, heaps, stacks — byte for byte"],
  ["FILESYSTEM", "open files, descriptors, the workload root"],
  ["NETWORK", "sockets where CRIU permits; reconnect otherwise"],
  ["DEVICE", "GPU contexts on matching checkpoint-capable hardware"],
  ["ENVIRONMENT", "variables, working directory, identity"],
];

export function StateInventory() {
  return (
    <section className="landing-band">
      <div className="shell split-layout">
        <Reveal variant="rise">
          <div className="section-intro">
            <p className="eyebrow">State inventory</p>
            <h2>
              What moves
              <br />
              is the whole process.
            </h2>
            <p className="band-lede">
              Not a file. Not a container image. The running thing itself — captured, transferred,
              and reconstructed.
            </p>
          </div>
        </Reveal>
        <div className="boundary-list">
          {MOVES.map(([k, v], index) => (
            <Reveal key={k} delay={index * 55} variant="pour">
              <div className="move-row">
                <span>{k}</span>
                <p>{v}</p>
              </div>
            </Reveal>
          ))}
        </div>
      </div>
    </section>
  );
}

const SAFETY_LINES: TermLine[] = [
  { kind: "cmd", text: "shiftgate migrate demo --to gpu-west --preflight" },
  { kind: "out", text: "compatibility report · dry run · workload not frozen" },
  { kind: "cmd", text: "shiftgate migrate demo --to gpu-west" },
  { kind: "out", text: "restored on gpu-west · health check passed · committing" },
  { kind: "out", text: "source retired only after the destination validated" },
];

export function CommitGate() {
  return (
    <section className="landing-band evidence-band">
      <div className="shell evidence-layout">
        <Reveal variant="rise">
          <div>
            <p className="eyebrow accent">The commit gate</p>
            <h2>
              A move is a state machine,
              <br />
              not a leap of faith.
            </h2>
            <p className="band-lede">
              Compatibility is checked before state moves. In live mode the source keeps running
              until the freeze — and it is retired only after the destination restores and passes
              its health check. Any failure rolls back with the source preserved.
            </p>
            <Link href="/docs/migration" className="text-link">
              Read the migration model <span aria-hidden="true">→</span>
            </Link>
          </div>
        </Reveal>
        <Reveal variant="settle" delay={120}>
          <TerminalFlow
            lines={SAFETY_LINES}
            label="shiftgate migrate — dry run, then commit"
            caption="command surface · output abbreviated"
          />
        </Reveal>
      </div>
    </section>
  );
}

const USES: Array<{ k: string; title: string; body: string; cmd: string }> = [
  {
    k: "ML HOSTS",
    title: "Evacuate a GPU node mid-run",
    body: "Move a warm training workload to another machine in your estate for maintenance or rebalancing. Its memory and process state travel with it, so the run continues instead of starting over.",
    cmd: "shiftgate migrate train-01 --to gpu-west",
  },
  {
    k: "CI RUNNERS",
    title: "Warm once. Run many.",
    body: "Clone one warmed checkpoint into independent workers on a compatible host — each worker restores from the checkpoint instead of re-warming a build environment per job.",
    cmd: "shiftgate clone CHECKPOINT_ID --count 10 --prefix ci",
  },
  {
    k: "DEV STACKS",
    title: "Park the stack. Resume it whole.",
    body: "Checkpoint a long-running dev environment and bring it back on this machine or another. Lazy restore starts the process before every memory page has landed.",
    cmd: "shiftgate restore CHECKPOINT_ID --lazy",
  },
  {
    k: "FAILOVER",
    title: "A warm standby that acts when the source dies",
    body: "Checkpoints replicate to a standby agent. The newest copy restores on your command — automatic restore additionally needs a fencing provider (disabled by default), and refuses without one.",
    cmd: "shiftgate workload failover demo --to https://standby:8443",
  },
];

export function UseCases() {
  return (
    <section className="landing-band">
      <div className="shell">
        <Reveal variant="rise">
          <div className="section-intro wide">
            <p className="eyebrow accent">Where the restart is too expensive</p>
            <h2>Workloads that hurt to rebuild.</h2>
          </div>
        </Reveal>
        <div className="use-grid">
          {USES.map((u, index) => (
            <Reveal key={u.k} delay={index * 80} variant="rise" className="use-reveal">
              <article className="use-case">
                <span className="use-k">{u.k}</span>
                <h3>{u.title}</h3>
                <p>{u.body}</p>
                <code className="use-cmd">{u.cmd}</code>
              </article>
            </Reveal>
          ))}
        </div>
      </div>
    </section>
  );
}

function Boundary({ index, title, body, tone }: { index: string; title: string; body: string; tone: string }) {
  return (
    <div className={`boundary boundary-${tone}`}>
      <span>{index}</span>
      <div>
        <h3>{title}</h3>
        <p>{body}</p>
      </div>
    </div>
  );
}

export function BoundaryBand() {
  return (
    <section className="landing-band light-band">
      <div className="shell split-layout">
        <Reveal variant="rise">
          <div className="section-intro">
            <p className="eyebrow">The honest boundary</p>
            <h2>
              Not VM magic.
              <br />
              Real process state.
            </h2>
            <p className="band-lede">SHIFT moves the workload, not the machine. That line is the product.</p>
          </div>
        </Reveal>
        <div className="boundary-list">
          <Boundary
            index="01"
            title="It is"
            body="CRIU-level checkpoint/restore: process trees, memory pages, open files, and environment — encrypted, chunked, and reconstructed on a machine you own."
            tone="good"
          />
          <Boundary
            index="02"
            title="It is not"
            body="Virtual-machine migration. No hypervisor, no shared storage, no whole-OS image. The destination may run a different distro or kernel — subject to the compatibility report."
            tone="warn"
          />
          <Boundary
            index="03"
            title="It does not pretend"
            body="A live move still freezes the workload for the final checkpoint — measured and reported as downtime. Transparent TCP state only where CRIU TCP repair is permitted."
            tone="muted"
          />
        </div>
      </div>
    </section>
  );
}

const VERBS: Array<{ n: string; title: string; body: string; cmd: string; href: string }> = [
  {
    n: "01",
    title: "Checkpoint",
    body: "Park long-running work into an encrypted, content-addressed state.",
    cmd: "checkpoint create",
    href: "/docs/checkpoints",
  },
  {
    n: "02",
    title: "Move",
    body: "Transfer only what the destination is missing over authenticated peer channels.",
    cmd: "migrate --to",
    href: "/docs/migration",
  },
  {
    n: "03",
    title: "Clone",
    body: "Turn a warm state into independent workers on one compatible host.",
    cmd: "clone --count",
    href: "/docs/concepts",
  },
  {
    n: "04",
    title: "Recover",
    body: "Rebuild from a verified root checkpoint when hardware or capacity changes.",
    cmd: "restore --lazy",
    href: "/docs/migration",
  },
];

export function Primitives() {
  return (
    <section className="landing-band">
      <div className="shell">
        <Reveal variant="rise">
          <div className="section-intro wide">
            <p className="eyebrow accent">Built for the expensive restart</p>
            <h2>Four verbs. One continuity layer.</h2>
          </div>
        </Reveal>
        <div className="verb-grid">
          {VERBS.map((v, index) => (
            <Reveal key={v.n} delay={index * 90} variant="settle" className="verb-reveal">
              <Link href={v.href} className="verb">
                <span>{v.n}</span>
                <b aria-hidden="true">↗</b>
                <h3>{v.title}</h3>
                <p>{v.body}</p>
                <code className="verb-cmd">shiftgate {v.cmd}</code>
              </Link>
            </Reveal>
          ))}
        </div>
      </div>
    </section>
  );
}

export function FinalCTA() {
  return (
    <section className="landing-band final-band">
      <div className="shell">
        <div className="final-layout">
          <Reveal variant="rise">
            <div>
              <p className="eyebrow accent">Bring one hard workload</p>
              <h2>
                Prove continuity
                <br />
                on your machines.
              </h2>
              <p className="band-lede">
                Install the agent, run the doctor, and checkpoint a real process before you commit to
                a fleet rollout.
              </p>
            </div>
          </Reveal>
          <div className="final-actions">
            <Link href="/download" className="button button-primary">
              Download SHIFT <span aria-hidden="true">↗</span>
            </Link>
            <Link href="/docs/quickstart" className="button button-quiet">
              Read the quickstart <span aria-hidden="true">→</span>
            </Link>
          </div>
        </div>
        <div className="code-block install-line">
          <span className="term-green">$ </span>
          curl -fsSL https://github.com/Fed-Labs/shiftgate/releases/latest/download/install.sh | bash
        </div>
        <p className="final-footnote">
          Linux x86_64 only, with a privileged agent and a CRIU-capable kernel. Automatic failover
          needs a fencing provider (disabled by default — fail-closed); the manual trigger can run
          two live copies if the source is partitioned rather than dead. Pre-1.0: a
          MINOR bump may break automation; releases keep an N-1 interop and rollback window.
        </p>
      </div>
    </section>
  );
}
