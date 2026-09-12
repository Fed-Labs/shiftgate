# SHIFT Benchmarks

Every number on this page was measured by the system itself — checkpoint
manifests and migration records carry each metric as it happens, so nothing
here is a projection, an estimate, or a number a poller approximated
afterwards. Ranges are three consecutive runs on one machine; the benchmark
source lives in the repository and the exact commands to reproduce each
table are at the end.

## Environment

- One machine, 12 logical CPUs. Both `shift-agent` processes and the
  workloads ran inside one privileged `golang:1.24-bookworm` container with
  CRIU 3.17.1 on a 6.12 kernel.
- The two agents are separate processes with separate state directories,
  identities, keys, and cgroup roots — but they share the host's CPUs and
  one NVMe-backed filesystem. The destination is not a second machine with
  its own disk.
- Migration traffic crosses a real TLS 1.3 channel over `127.0.0.1` — a
  loopback network. Loopback flatters transfer *durations*; the byte counts
  are the network-independent quantity a real deployment pays inside its
  freeze window, and both are reported.
- Chunks are 4 MiB, zstd at its default level (the gzip column below is the
  codec's legacy path, kept readable for old chunks).

## The workload

One `python3` process holding a ~96 MiB heap plus a 64 MiB data directory
(three-quarters text-shaped, one-quarter freshly random 256 KiB blocks),
writing one progress line per second — quiescent between lines, like a
service at rest. Two heap variants, identical in everything but
compressibility:

| Variant | Heap contents | What it stands for |
|---|---|---|
| compressible | repeated text blocks | logs, caches, structured state — zstd shrinks it ~100:1 |
| incompressible | random bytes | databases, ML weights, binary state — zstd cannot shrink it |

## What each migration metric means

| Metric | Window |
|---|---|
| `downtime` | the workload's freeze instant → the destination's restore call returned with health validated |
| `checkpoint` | the final checkpoint's own duration; in live mode this is the final delta dump |
| `transfer`, rate | the frozen window only — pre-copy bytes are excluded |
| `pre-copy` | bytes moved by pre-copy passes while the workload kept running |
| `deduplicated` | plaintext bytes of manifest chunks the destination already held when the manifest was imported (live: the chunks the passes delivered) |

## Checkpoint and restore

A full stopped checkpoint of the compressible workload and the synchronous
restore call that follows it.

| Metric | 3 runs |
|---|---|
| Plain state | 171,008,000 B (163 MiB) |
| Stored after zstd + encryption | 17,483,863 – 17,484,627 B (**10.2%**) |
| Chunks | 42 |
| Checkpoint duration | 914 – 973 ms |
| Restore (wall, includes resume + health validation) | 2.27 – 2.77 s |

The compression ratio is the mixed data directory's honest floor, not the
heap's: three-quarters of the payload is text and one quarter is fresh
random bytes that cannot shrink, so 10.2% is what a real mixed workload
stores, not a best case.

## Lazy restore

`restore --lazy` starts the process before its memory is materialized — a
`criu lazy-pages` daemon serves faults on demand and streams the rest in the
background. The benchmark restores the same workload twice per run from
equivalent stopped checkpoints, once lazy and once eager, so both numbers
come from the same machine state.

| Metric | 3 runs |
|---|---|
| Time to first execution, lazy | 142 – 179 ms |
| Time to first execution, eager | 96 – 131 ms |
| Restore wall (lazy) | 2.23 – 2.51 s |
| Restore wall (eager) | 2.10 – 2.36 s |
| Checkpoint after a lazy restore | 1.104 – 1.209 s |

Lazy restored *slower* to first execution than eager here, and that is the
honest result for this environment: the 17 MB stored image materializes from
local disk in ~100 ms, so there is nothing for laziness to hide, while the
lazy-pages daemon's startup and socket handshake (~50 ms) sits on the
critical path before `criu restore --lazy-pages` can connect. Where lazy
pays off is a memory whose materialization dominates startup — a heap of
tens of gigabytes, or slow storage under the image set — where the process
begins executing while the bulk streams behind it. The checkpoint after a
lazy restore (1.10 – 1.21 s against 914 – 973 ms for the same workload's
first checkpoint in the table above) pays part of the deferred page-in:
reading the process's memory faults the remaining pages in through the
daemon. Both rows report the same measure — the restore call's span from
start to the tree executing — so they compare honestly.

## Incremental checkpointing

A second checkpoint against a leave-running parent on unchanged state:

| Metric | 3 runs |
|---|---|
| Full checkpoint stored | 17,490,911 – 17,491,402 B, 959 – 995 ms |
| Incremental plain | 67,307,520 B |
| Incremental stored | 16,816,137 – 16,816,988 B |
| Deduplicated against parent | 62,914,560 B (**93.5%** of plain) |
| Incremental duration | 1.212 – 1.239 s |

The incremental image still carries the data directory — file-level change
stamping cannot prove an unmodified file's contents still match without
reading it — so its stored size is dominated by the payload, not the delta.
The 93.5% deduplication is the part a real repeated checkpoint saves: the
process image is negotiated against the parent and only missing chunks move.

## Migration — compressible heap

Cold stops the workload for the whole capture-and-transfer window. Live runs
pre-copy passes (cap 4; every run converged at 2 — the quiescent heap's
second delta fell below the convergence threshold) and freezes only for the
final delta.

| Metric | Cold (3 runs) | Live (3 runs) |
|---|---|---|
| Plain state | 171,008,000 B | 171,192,320 – 171,202,560 B |
| Transferred (stored) | 17,483,864 – 17,484,570 B (10.2%) | 17,511,870 – 17,512,607 B (10.2%) |
| — moved before the freeze | — | 718,611 – 719,378 B |
| — moved inside the freeze | = transferred | 16,793,219 – 16,793,266 B |
| Deduplicated at import | 0 B | 104,017,920 – 104,028,160 B |
| Checkpoint (final dump) | 875 ms – 1.00 s | 1.223 – 1.296 s |
| Transfer (frozen window) | 215 – 223 ms | 107 – 116 ms |
| Transfer rate | 74.7 – 77.4 MiB/s | 138 – 149 MiB/s |
| Restore | 2.115 – 2.253 s | 2.203 – 2.253 s |
| **Downtime** | **3.746 – 3.905 s** | **3.197 – 3.292 s** |

With a heap compression makes nearly free, pre-copy cannot shine: the
passes moved only 0.7 MB because the whole memory image compresses to that.
The frozen window still shrinks from 17.5 MB to 16.8 MB (the final memory
image is a delta, and the destination already holds the pass chunks), and
downtime improves by roughly half a second. Both modes are dominated by the
same fixed costs — the final dump and the ~2.2 s restore.

## Migration — incompressible heap

The same migration pair over a heap zstd cannot shrink. This is the shape
pre-copy exists for: the heap itself is the bulk of the state.

| Metric | Cold (3 runs) | Live (3 runs) |
|---|---|---|
| Plain state | 173,731,840 B | 173,916,160 – 173,926,400 B |
| Transferred (stored) | 118,056,117 – 118,057,515 B (68.0%) | 118,081,766 – 118,087,787 B (67.9%) |
| — moved before the freeze | — | 101,288,540 – 101,294,559 B |
| — moved inside the freeze | = transferred | 16,793,194 – 16,793,228 B |
| Deduplicated at import | 0 B | 106,741,760 – 106,752,000 B |
| Checkpoint (final dump) | 1.30 – 1.34 s | 1.807 – 2.154 s |
| Transfer (frozen window) | 430 ms – 1.078 s | 107 – 113 ms |
| Transfer rate | 104.5 – 261.9 MiB/s | 141.9 – 150.1 MiB/s |
| Restore | 2.337 – 2.439 s | 2.353 – 2.450 s |
| **Downtime** | **4.607 – 5.502 s** | **3.385 – 3.523 s** |

## What pre-copy actually buys

The incompressible pair is the honest measurement of spec §10's promise.
Cold must carry all 118 MB inside the freeze. Live's first pass moves
~101 MB of it while the workload keeps running; the frozen window is left
with 16.8 MB — the data directory plus a memory delta — a **7× reduction**
in the bytes a real network would have to move while the application is
stopped.

Loopback makes the downtime delta look small: 101 MB over localhost costs
under a second, so downtime improved by only ~1.2 – 2.0 s. On a real
network that same byte reduction is the whole story — at 100 MiB/s it is a
second of downtime avoided; at 10 MiB/s it is ten. The frozen-window byte
counts above, not the loopback durations, are the numbers to reason with.

The trade live mode pays is visible too: its final dump takes 1.8 – 2.2 s
against cold's 1.3 s (a delta dump still walks every page table against its
parent images), and its total wall clock is not shorter — the passes are
extra work done before the freeze so the freeze itself carries less. Live
migration buys downtime, not total time.

Before the first pass's chunks leave, the source reserves a measured upper
bound on the destination — the root tree's size plus pass 1's packaged size
times one more than the pass cap — so a destination without room for the
worst case refuses the migration before anything moves (~570 MB in these
runs). A workload that dirties memory faster than the passes converge can
still exceed the bound; the destination then rejects the transfer, the
source is preserved and still running, and the migration record says so.

A destination that dies mid-pass fails the migration the same way — source
untouched, workload running the whole time, and the record reports zero
downtime, because the workload was never frozen. That path is pinned by
`TestE2ELiveMigrationDestinationDiesDuringPreCopy` in the integration
suite.

## Chunk codec and chunk store

Throughput on one 4 MiB chunk of each payload shape, in isolation — no
encryption, no disk (`BenchmarkChunkCodec`):

| Payload | zstd compress | gzip compress (legacy) | zstd decompress |
|---|---|---|---|
| text | 2,643 – 3,284 MB/s | 1,953 – 2,128 MB/s | 3,493 – 4,992 MB/s |
| heap-mix (75% text / 25% random) | 1,796 – 2,193 MB/s | 1,026 – 1,101 MB/s | 3,350 – 4,703 MB/s |
| zeros | 3,284 – 3,675 MB/s | 2,016 – 2,201 MB/s | 3,978 – 5,575 MB/s |
| random | 672 – 960 MB/s | 555 – 570 MB/s | 4,270 – 10,604 MB/s |

The full path the agent pays per checkpoint, on one 4 MiB heap-mix chunk
(`BenchmarkChunkStore`): a fresh write is compress + encrypt + fsync +
content-addressed link at **294 – 364 MB/s**; re-storing identical state
verifies and skips at 271 – 315 MB/s; the restore path (read + digest +
decrypt + decompress + verify) runs at **559 – 704 MB/s**.

## Reproducing these numbers

The end-to-end tables need a privileged Linux container with CRIU — the
same environment the integration suite uses:

```bash
docker run --rm --privileged -v "$PWD":/src -w /src golang:1.24-bookworm \
  bash -c 'apt-get update -qq && apt-get install -y -qq criu procps >/dev/null 2>&1 &&
           SHIFT_TEST_E2E=1 go test ./tests/integration/ -run "^$" \
                -bench BenchmarkE2E -benchtime 1x -count 3 -v'
```

The codec and chunk-store tables run anywhere Go does:

```bash
go test ./internal/chunkstore/ -run "^$" -bench . -benchtime 1x -count 3
```

Every scenario boots real agents, real CRIU checkpoints, and a real TLS
peer channel, and prints the metrics from the migration and checkpoint
records themselves.

## What these numbers do not tell you

- **Nothing about real networks.** Loopback TLS measures CPU, not
  bandwidth or latency. The frozen-window byte counts are the transferable
  quantity; the durations are not.
- **Nothing about multi-tenant destinations.** Both agents shared one
  machine's CPUs and disk. A destination under load restores slower than
  these tables show.
- **Nothing about dirty workloads.** The benchmark heap is quiescent
  between progress lines, so passes converged at 2 and the frozen delta was
  tiny. A workload that dirties its heap continuously converges slower and
  keeps a larger final delta — pre-copy still helps, but by less.
- **Nothing about your data.** Compression ratios depend entirely on
  payload shape; the two heap variants here bracket the range, and your
  workload sits somewhere in it.
- **Single-machine variance.** Three consecutive runs on one box — the
  cold incompressible transfer swung 430 ms – 1.078 s across them. Treat
  every duration as an order of magnitude with error bars, not a constant.

