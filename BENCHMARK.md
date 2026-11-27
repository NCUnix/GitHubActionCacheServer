# Performance Benchmark

Go v1.2.3 delivered **2.9–3.5×** Rust v10.0.2's first-restore throughput
for eight concurrent 128 MiB caches across the six backend combinations.
Later restore throughput was within **9%**, and SeaweedFS upload throughput
for the same workload was within **8%**. In the backend comparison, Go peaked
at **44–54 MiB** RSS versus **102–126 MiB** for Rust, while Rust handled
**1.3–4.8×** as many metadata hits per second.

## Test Setup

| Item | Configuration |
| --- | --- |
| Go | `v1.2.3` Linux ARM64 binary built locally with Go 1.27.4 and the release workflow flags |
| Rust | Upstream `v10.0.2` Linux ARM64 release binary |
| Hosts | Separate server and client on `c6gn.2xlarge`, 8 vCPUs each |
| OS and storage | Amazon Linux 2023, XFS on gp3 provisioned for 3,000 IOPS and 500 MiB/s |
| Transport | HTTP/1.1 over private networking |
| Database | SQLite WAL, one Go writer and up to eight Go readers |
| Application settings | One server process, cleanup and token signature verification disabled, stdout discarded |
| Load generator | Go Cache v2 JSON/Twirp client, 4 MiB blocks, up to four parallel block PUTs per cache |

Each configuration ran three times with a fresh database and cache directory
or bucket for each run. The Go retest used the same workloads and host type
as the earlier Rust runs.
Tables use the median across runs unless stated otherwise. Every measured
cache restore passed a byte-for-byte check.
Bold marks the best result for each metric. When a cell contains two metrics,
each is compared independently.

### Durability Settings

**Go FULL** uses Go's defaults, `STORAGE_FILESYSTEM_FSYNC=true` and
`DB_SQLITE_SYNCHRONOUS=FULL`. **Go NORMAL** disables file `fsync` and sets
`DB_SQLITE_SYNCHRONOUS=NORMAL`. The Go release uses SQLite's default
1,000-page WAL auto-checkpoint. **Rust v10.0.2** defaults to SQLite `NORMAL`
with a 10,000-page auto-checkpoint. Its filesystem uploader calls `flush()`
before publishing the file and skips file `fsync`.

## Payload Size

Caches run sequentially and blocks within a cache can upload in parallel.
The 1, 32, and 128 MiB tests use nine, five, and three entries per run, respectively.

**Each cell shows MiB/s / p95 latency (ms).**

| Operation | Go v1.2.3 FULL | Go v1.2.3 NORMAL | Rust v10.0.2 |
| --- | ---: | ---: | ---: |
| Upload 1 MiB | 49 / 24.27 | 221 / 11.90 | **388 / 4.15** |
| First restore 1 MiB | 201 / 6.82 | 596 / 2.73 | **690 / 1.84** |
| Upload 32 MiB | 665 / 49.97 | 2,414 / 13.50 | **2,624 / 12.60** |
| First restore 32 MiB | 958 / 34.88 | **1,120 / 28.65** | 887 / 38.88 |
| Upload 128 MiB | 930 / 139.27 | 2,701 / 47.73 | **2,832 / 45.76** |
| First restore 128 MiB | 1,081 / 119.51 | **1,132 / 113.22** | 985 / 131.78 |

## Concurrency Scaling

Each cache is 16 MiB.
The concurrency 1, 8, and 32 tests use 8, 24, and 64 entries per run.

**Each cell shows upload / first restore (MiB/s).**

| Concurrent caches | Go v1.2.3 FULL | Go v1.2.3 NORMAL | Rust v10.0.2 |
| ---: | ---: | ---: | ---: |
| 1 | 537 / 834 | 2,105 / **1,107** | **2,478** / 852 |
| 8 | 735 / 2,664 | 2,619 / **2,913** | **2,930** / 2,132 |
| 32 | 575 / 2,754 | 2,168 / 2,912 | **2,375 / 2,930** |

The 32-concurrent workload transfers 1 GiB in each direction. CPU is server
process CPU time divided by client-measured phase duration, expressed in cores.
Process writes are the change in `/proc/<pid>/io` `write_bytes`. Device writes
are the change in the root block device's written-sector counter, including
filesystem metadata and other host writes.

**32 concurrent caches, 1 GiB per direction.**

| Concurrency-32 metric | Go v1.2.3 FULL | Go v1.2.3 NORMAL | Rust v10.0.2 |
| --- | ---: | ---: | ---: |
| Upload p95 (ms) | 1,390.60 | **364.99** | 386.86 |
| First restore p95 (ms) | 362.98 | 337.86 | **337.04** |
| Upload CPU (cores) | **0.70** | 4.06 | 4.78 |
| First restore CPU (cores) | **1.40** | 1.62 | 3.29 |
| Upload process writes (MiB) | 1,036.16 | 1,033.32 | **1,030.69** |
| First restore process writes (MiB) | 2.18 | **1.63** | 2.93 |
| Upload device writes at response completion (MiB) | 1,047.19 | 8.61 | 0.17 |
| Upload device writes after flush (MiB) | 1,054.02 | 1,040.64 | **1,038.05** |
| First restore device writes at response completion (MiB) | 2.76 | 0.00 | 0.00 |
| First restore device writes after flush (MiB) | 10.65 | **8.71** | 11.07 |

The harness flushes the filesystem after upload and waits five seconds after
restore before flushing again. Both flushes are outside the timed phases.
Response-time device writes show when writes reach the device, so those rows
carry no ranking. Most writes in Go NORMAL and Rust reach the device after
upload responses complete. Restore measurements use a warm page cache and
include background work through the post-restore sample.

## Startup and Memory

Startup measures time from launching the process to the first successful
`/health` poll, with a 20 ms polling interval and five fresh databases per
configuration. Idle RSS is sampled at readiness. Transfer peak RSS uses
the process high-water mark through the 32-concurrent restore, including the
preceding health workload.

**Five startup samples per configuration.**

| Metric | Go v1.2.3 FULL | Go v1.2.3 NORMAL | Rust v10.0.2 |
| --- | ---: | ---: | ---: |
| Uncompressed server binary (MiB) | 45.13 | 45.13 | **14.55** |
| Cold start p50 / p95 (ms) | **31** / 34 | **31 / 31** | **31 / 31** |
| Server processes | 1 | 1 | 1 |
| Idle RSS p50 (MiB) | 33.14 | 33.33 | **11.12** |
| Transfer peak RSS (MiB) | **47.81** | 55.96 | 176.25 |

## HTTP and Metadata

The client sends 2,000 `/health` requests at concurrency 1 and 100,000 at
concurrency 64. Requests reuse connections. Health measurements precede
cache transfers.

**HTTP `/health` results.**

| Health metric | Go v1.2.3 FULL | Go v1.2.3 NORMAL | Rust v10.0.2 |
| --- | ---: | ---: | ---: |
| 1 connection (requests/s) | 8,030 | 8,196 | **10,102** |
| 1 connection p99 (ms) | 0.165 | 0.161 | **0.122** |
| 64 connections (requests/s) | 122,708 | 120,278 | **130,475** |
| 64 connections p99 (ms) | 2.156 | **2.120** | 2.136 |
| 64 connections CPU (cores) | 4.72 | 4.67 | **2.36** |
| 64 connections (requests/s/core) | 25,840 | 25,773 | **56,180** |
| 64 connections peak RSS (MiB) | 42.67 | 42.91 | **14.39** |

The `GetCacheEntryDownloadURL` API hits include the implementation's storage
existence checks. Each run uses 1,000 requests per outcome at concurrency 1
and 5,000 at concurrency 32.

**Each cell shows requests/s / p95 latency (ms).**

| Metadata operation | Go v1.2.3 FULL | Go v1.2.3 NORMAL | Rust v10.0.2 |
| --- | ---: | ---: | ---: |
| Hit, concurrency 1 | 2,251 / 0.479 | 2,234 / 0.479 | **3,216 / 0.330** |
| Miss, concurrency 1 | 1,524 / 0.686 | 1,517 / 0.693 | **2,976 / 0.351** |
| Hit, concurrency 32 | 20,257 / 2.955 | 20,157 / 3.038 | **23,831 / 1.718** |
| Miss, concurrency 32 | 12,526 / 4.612 | 12,554 / 4.685 | **19,973 / 1.919** |

## 32 MiB Block Workload

An additional run used the same three configurations and 32 MiB blocks.
The 8×128 MiB workload uses four parallel blocks per cache. The 32×16 MiB
workload uses one block per cache.

**Median of three runs.**

| Metric | Go v1.2.3 FULL | Go v1.2.3 NORMAL | Rust v10.0.2 |
| --- | ---: | ---: | ---: |
| Upload 8×128 MiB (MiB/s) | 807 | 2,923 | **2,940** |
| Upload 32×16 MiB (MiB/s) | 872 | 2,855 | **2,892** |
| Upload 32×16 MiB p95 (ms) | 585.35 | 178.22 | **176.37** |
| Metadata hits at concurrency 32 (requests/s) | 20,642 | 20,410 | **25,833** |
| Peak RSS (MiB) | 48.12 | **47.44** | 126.40 |

The first restore starts after upload, the immediate repeat follows it, and
the later restore starts five seconds after the repeat.

**Each cell shows MiB/s / p95 latency (ms).**

| Restore phase | Go v1.2.3 FULL | Go v1.2.3 NORMAL | Rust v10.0.2 |
| --- | ---: | ---: | ---: |
| 8×128 MiB, first | 2,740 / 373.67 | **2,873 / 356.34** | 937 / 1,092.40 |
| 8×128 MiB, immediate repeat | 2,859 / 358.12 | **2,918 / 350.83** | 509 / 2,011.75 |
| 8×128 MiB, later | 2,817 / 363.47 | **2,930 / 349.41** | 2,843 / 360.02 |
| 32×16 MiB, first | 2,659 / 190.80 | 2,882 / 174.47 | **2,909 / 171.80** |
| 32×16 MiB, immediate repeat | 2,895 / 174.42 | 2,936 / **172.12** | **2,949** / 173.19 |

For 8×128 MiB, Rust rose from 937 MiB/s on the first restore to 2,843 MiB/s
on the later restore. Go NORMAL delivered 2,873 and 2,930 MiB/s on those
same phases.

## Backend Comparison

Go uses file `fsync` disabled, SQLite `FULL` where applicable, and streaming
S3 PUT. The runs use PostgreSQL 17.12, MySQL 8.4.13.1, and SeaweedFS 4.55
on a separate backend host. Each run uses a fresh database and cache directory
or bucket. S3 restores pass through the cache server.

**Go v1.2.3 / Rust v10.0.2 (MiB/s).**

| Database | Storage | Upload 8×128 MiB | First restore 8×128 MiB | Later restore 8×128 MiB |
| --- | --- | ---: | ---: | ---: |
| SQLite | Filesystem | 2,358 / **2,929** | **2,722** / 940 | 2,741 / **2,906** |
| SQLite | SeaweedFS | 603 / **644** | **2,101** / 660 | 2,730 / **2,890** |
| PostgreSQL | Filesystem | 2,734 / **2,835** | **2,844** / 940 | 2,874 / **2,925** |
| PostgreSQL | SeaweedFS | **649** / 601 | **1,812** / 623 | 2,710 / **2,849** |
| MySQL | Filesystem | **2,463** / 2,438 | **2,744** / 923 | 2,800 / **2,884** |
| MySQL | SeaweedFS | **597** / 595 | **2,120** / 609 | 2,550 / **2,782** |

**Go v1.2.3 / Rust v10.0.2.**

| Database | Storage | Upload 32×16 MiB (MiB/s) | Metadata hits (requests/s) | Hit p95 (ms) | Peak RSS (MiB) |
| --- | --- | ---: | ---: | ---: | ---: |
| SQLite | Filesystem | 1,089 / **2,889** | 20,068 / **26,466** | 3.04 / **1.57** | **48** / 124 |
| SQLite | SeaweedFS | **468** / 378 | 10,513 / **27,191** | 5.45 / **1.51** | **54** / 106 |
| PostgreSQL | Filesystem | 2,459 / **2,726** | 5,240 / **14,733** | 21.27 / **2.41** | **47** / 126 |
| PostgreSQL | SeaweedFS | 341 / **364** | 2,771 / **13,278** | 29.70 / **2.68** | **50** / 106 |
| MySQL | Filesystem | 1,936 / **2,418** | 5,004 / **11,891** | 23.37 / **2.93** | **44** / 124 |
| MySQL | SeaweedFS | **357** / 341 | 2,759 / **10,503** | 30.77 / **3.25** | **48** / 102 |

### SeaweedFS Upload Stages

Both implementations use a single S3 PUT per 32 MiB block in the
8×128 MiB workload. Go treats block IDs as opaque and uses the client's
`blocklist` to determine the final order. Its S3 implementation commits
the ordered block references without copying objects.
Rust requires a block ID format that encodes the part index, writes to the
final key on upload, and acknowledges `blocklist` after checking authorization.

**Go v1.2.3 / Rust v10.0.2, per-cache p95 (ms).**

| Database | Create | Block PUTs | `blocklist` | Finalize | Whole upload |
| --- | ---: | ---: | ---: | ---: | ---: |
| SQLite | 38.42 / **2.17** | 1,659.76 / **1,587.42** | 21.49 / **0.28** | 21.94 / **2.18** | 1,697.02 / **1,591.12** |
| PostgreSQL | 13.72 / **13.43** | **1,560.61** / 1,687.21 | 6.39 / **0.63** | 9.71 / **8.23** | **1,578.20** / 1,704.48 |
| MySQL | **31.16** / 59.26 | 1,678.47 / **1,650.38** | 21.14 / **0.64** | 21.18 / **13.57** | **1,715.94** / 1,721.77 |

Block PUTs now account for most of the measured upload time. Stage p95
values are calculated independently.

The Node.js and earlier Go results are in the
[archived benchmark](https://github.com/NCUnix/GitHubActionCacheServer/blob/2af8c4a162458ae462ef40c07182430117c9c5b9/BENCHMARK.md).
