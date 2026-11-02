import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "TCP Repair or It Didn't Happen: Honest Socket Migration",
  description:
    "What it really takes to migrate live TCP connections: CRIU TCP repair, when socket state ports and when it is rejected, and why designing for reconnect is still the right default.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="CRIU deep dive"
      title="TCP Repair or It Didn't Happen: Honest Socket Migration"
      lede="Every 'transparent migration' claim eventually meets a socket. Transparent TCP state across machines requires CRIU's TCP repair mode — and even then, portability is a per-kernel question. Here is the honest version."
      sections={[
        {
          heading: "What TCP repair actually is",
          blocks: [
            { kind: "p", text: "TCP repair is a kernel mode that lets a privileged process seize a socket's internal state — sequence numbers, buffers, connection tuple — so it can be checkpointed and re-established on another host without a reconnect handshake. CRIU uses it to make live socket state part of the checkpoint image. It is powerful, privileged, and not universally available: the kernel must permit it, and the socket must be in a state repair can capture." },
            { kind: "code", body: "shiftgate checkpoint create app --tcp-state   # request CRIU TCP repair state\nshiftgate migrate app --to dst --preflight     # reports what will not port" },
          ],
        },
        {
          heading: "The portability tri-state",
          blocks: [
            { kind: "p", text: "TCP and device state are marked portable, adaptable, or external — unsupported state is rejected or surfaced as a limitation, never faked. Preflight carries the same verdict the real move would reach, so a rejected socket is a no-op, not a mid-move surprise." },
            { kind: "table", head: ["Verdict", "Meaning", "Your move"], rows: [
              ["Portable", "State captures and restores (repair permitted).", "Migrate; verify with a drill."],
              ["Adaptable", "Restores with reconnect/repair semantics.", "Design the client path for it."],
              ["External", "Cannot travel (foreign state, forbidden kernel path).", "Rebind/reconnect; keep the source alive."],
            ] },
          ],
        },
        {
          heading: "Design for reconnect anyway",
          blocks: [
            { kind: "bullets", items: [
              "Clients should tolerate a dropped connection on a scheduled window regardless — it is the cheapest insurance against every other failure too.",
              "Idempotent request handling makes both reconnect and rollback clean.",
              "Measure your own state: run the preflight against the real workload before promising anyone transparent sockets.",
            ] },
          ],
        },
      ]}
      honestEdges={[
        "We never claim transparent TCP migration without CRIU TCP repair — the claim follows the kernel.",
        "Unsupported state is rejected or surfaced at preflight; it is never silently dropped.",
        "Linux x86_64; privileged agent (repair is a privileged kernel facility).",
        "A move is still a measured freeze reported as downtime even with perfect socket portability.",
      ]}
      faqs={[
        { q: "Do all my TCP connections survive a move?", a: "Only those the kernel lets CRIU capture via repair, and only where preflight agrees. Anything else is reported as not portable before the move starts." },
        { q: "Why is repair privileged?", a: "It hands a process the internals of the TCP stack — exactly the kind of power the privileged agent model exists to contain." },
        { q: "Should I just build for reconnect?", a: "Yes, as the default. Migrating sockets is the optimization; tolerating reconnect is the architecture." },
      ]}
      cta={{ heading: "Run preflight on your sockets", body: "It answers the portability question without moving anything." }}
      related={[
        { href: "/guides/criu-limitations-explained", label: "CRIU limitations explained" },
        { href: "/guides/migrate-running-process-between-linux-servers", label: "Migrate a running process between Linux servers" },
        { href: "/docs/ci-burst", label: "The CI-burst proof kit" },
      ]}
    />
  );
}
