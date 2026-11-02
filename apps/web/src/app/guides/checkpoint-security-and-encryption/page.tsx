import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Checkpoint Security: Encryption, Trust Anchors, and Threat Model",
  description:
    "How checkpoint state is protected: content-addressed AES-256-GCM chunks, signed manifests, keyed addresses, mTLS peer channels, agent-held keys, and the threat model boundaries that keep plaintext away from the platform.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Guide"
      title="Checkpoint Security: Encryption, Trust Anchors, and Threat Model"
      lede="A checkpoint is the most sensitive artifact your infrastructure can hold: a full process image. This guide walks the actual protections — encryption, addressing, signing, transport, key custody — and the threat-model boundaries we do not cross."
      sections={[
        {
          heading: "What a checkpoint is, cryptographically",
          blocks: [
            { kind: "bullets", items: [
              "State compresses, encrypts, and splits into content-addressed chunks: AES-256-GCM, key derivation via HKDF-SHA256 (standard, off-the-shelf primitives — nothing bespoke).",
              "Content addresses are keyed, so plaintext hashes are not exposed to storage or observers.",
              "Manifests are signed with the machine's Ed25519 identity and stored as authenticated encrypted envelopes; corrupt or unauthenticated chunks fail closed — a tampered checkpoint restores nothing.",
              "Keys are generated and held by the agent in its state directory (0600). Workload keys never leave the agent; the platform stores metadata and authorization state only.",
            ] },
          ],
        },
        {
          heading: "Transport and trust anchors",
          blocks: [
            { kind: "p", text: "Remote agent listeners require mutual TLS unless explicitly started in development mode — agent-to-agent peer channels are mTLS throughout. Machine identity is Ed25519; trust anchors govern which peers and ingests are accepted, and non-peer ingest is admitted only through trust-anchor policy. Checkpoint plaintext is never available to the hosted service; keys stay with your agents (terms §7 says the same in legal language)." },
          ],
        },
        {
          heading: "Mirroring and hosted storage",
          blocks: [
            { kind: "p", text: "When mirroring is enabled (local disk, S3-compatible, or the control-plane backend), the remote side receives only chunk ciphertext and the encrypted manifest envelope — plus object-key and size metadata. Quota is enforced fail-closed from metered bytes. There is no customer-managed-KMS integration today: organizations that need one should keep mirroring on their own object store." },
          ],
        },
        {
          heading: "The master-key boundary (production requirement)",
          blocks: [
            { kind: "p", text: "At-rest confidentiality of the key hierarchy depends on the master-key keyfile. Without one, master keys sit in the state directory protected only by filesystem permissions. Production deployments must set SHIFT_MASTER_KEY_FILE: master keys are then sealed under a key derived from the keyfile. Treat the keyfile as the key boundary — back it up separately from the state directory." },
          ],
        },
        {
          heading: "Threat model boundaries",
          blocks: [
            { kind: "bullets", items: [
              "In scope: a stolen disk, a stolen backup, a curious storage provider, a tampered checkpoint in transit or at rest — all fail closed or see ciphertext only.",
              "Out of scope: a rooted agent host. Root on the agent owns the keys by design — that is what the privileged agent is.",
              "Out of scope: side channels and hardware attacks on the workload while it runs.",
              "Control plane: Argon2id password hashing with a separate pepper; opaque tokens (no JWTs anywhere) persisted only as keyed HMAC digests.",
            ] },
            { kind: "p", text: "The full documents: docs/security.md (mechanisms), docs/threat-model.md (assets, adversaries, boundaries), and the export classification note for the crypto in the distribution." },
          ],
        },
      ]}
      honestEdges={[
        "No customer-managed keys or external KMS integration today.",
        "A rooted agent host owns the workload keys — the trust anchor is the host, not the platform.",
        "SHIFT_MASTER_KEY_FILE is a production requirement; without it, at-rest key protection is filesystem permissions only.",
        "The hosted control plane sees metadata and authorization state — never checkpoint plaintext.",
      ]}
      faqs={[
        { q: "Can the platform read my checkpoint contents?", a: "No. Chunk ciphertext and encrypted manifests only; workload keys stay with your agents. The service stores metadata and authorization state." },
        { q: "What happens if someone tampers with stored chunks?", a: "Authenticated encryption and signed manifests make tampering detectable: corrupt or unauthenticated chunks fail closed instead of restoring garbage." },
        { q: "Is this compliant with <framework>?", a: "We publish the mechanisms and the threat model rather than a checkbox claim — no SOC 2/ISO marketing this year. Audit and SSO/SCIM ship on Business for teams building their own evidence." },
      ]}
      cta={{ heading: "Read the threat model", body: "Then run the proof on your own hardware — the security story is only useful if the mechanics hold." }}
      related={[
        { href: "/guides/criu-limitations-explained", label: "CRIU limitations explained" },
        { href: "/guides/air-gapped-workload-migration", label: "Workload migration in air-gapped environments" },
        { href: "/docs/security", label: "The security page" },
      ]}
    />
  );
}
