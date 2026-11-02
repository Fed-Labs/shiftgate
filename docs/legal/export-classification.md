# Encryption Export Self-Classification

**Effective date:** 2026-10-14

SHIFT implements standard, publicly documented cryptography only:
HKDF-SHA256 key derivation, AES-256-GCM authenticated encryption for
checkpoint chunks and state files, Ed25519 signatures for manifests and
releases, and TLS 1.3 (via the Go standard library) for transport. The full
inventory is [docs/security.md](../security.md). There is no proprietary or
novel cryptographic construction anywhere in the tree.

Self-classification statement (United States, EAR §740.17 / License Exception
TSU mass-market criteria; equivalent EU/UK treatment under Regulation (EU)
2021/821 Annex I Category 5 Part 2 "cryptography note"):

- **Product:** SHIFT / SHIFTGATE agents, CLI, control plane, and relay.
- **Cryptography:** open-source encryption for data in transit and at rest;
  key lengths ≤ 256 bits for symmetric operations; standard public-key
  primitives (Ed25519, and the platform TLS stack) for authentication and
  transport.
- **Classification:** 5D002 / 5D992.c equivalent; eligible for License
  Exception ENC/TSU mass-market treatment. The source code is publicly
  available, the cryptographic implementation is standard-library and
  documented, and no custom cryptanalysis-resistant functionality exists.
- **Key management:** keys are generated and held by the customer's agents
  (self-hosted) or by the platform's agent state for the hosted service;
  there is no key escrow and no third-party access to plaintext keys.
  Customer-managed KMS integration is **not** implemented — do not represent
  it in export filings.

This note is provided for compliance questionnaires and customs friction
reduction. It is not formal legal advice; enterprise exporters should obtain
their own classification for their distribution channel.
