# The e2e rig

An INN2 news server filled with generated fixture sets, the streamer pointed at
it, and a client reading files through webdav and the FUSE mount, timing what
comes back. Driven by `go run ./test/run`, which shells out to `docker compose`;
no state outside `test/build/`.

```sh
go run ./test/run -build            # first run: build the images too
go run ./test/run -probe-only       # what this host can actually impose
go run ./test/plot                  # summary.csv -> summary.svg
```

Needs Docker, a few GiB of free memory, and for the fuse tests a daemon granting
`/dev/fuse` and `SYS_ADMIN`. A run generates payloads and archives, posts them
and writes one nzb per set, then per cell restarts the streamer, adds the nzbs
and reads. Posting is skipped when the spool already holds the current fixtures
(the `build/posted.stamp` hash matches and the news server is still healthy), so
a `-keep` rig is refilled with metadata only. Interrupting cancels rather than
kills, so the CSVs still get written.

## Fixtures

A test is a fixture set plus one file path in it, named `<transport>-<fixture>`
(`webdav-*` over HTTP, `fuse-*` through the mount).
Every selected test runs the whole matrix. Full-file reads are checked against the payload digest.

| Fixture          | What it tests                                            |
| ---------------- | -------------------------------------------------------- |
| `plain`          | file posted directly, no archive                         |
| `rar-stored`     | as stored member of a rar                                |
| `rar-multi`      | a multi-volume stored set                                |
| `rar-compressed` | a compressed member, decoder stream                      |
| `rar-solid`      | solid (`-ms -m3`), the worst seek case                   |
| `7z`, `zip`      | presented as containers, measured whole, never validated |
| `par2`           | content plus recovery files                              |

### Special cases
| Fixture                   | What it tests                  |
| ------------------------- | ------------------------------ |
| `damaged`                 | a partly deleted file          |
| `webdav-plain-concurrent` | two `plain` files in parallel. |

## `test/run` flags                                   |

Test-Rig-Setup:

| Flag          | Options                     | Default       | What                                                                                                                                                                                                                     |
| ------------- | --------------------------- | ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `-ram`        | `true`/`false`              | `true`        | `true` puts the news spool and cache in tmpfs, several GiB of host RAM, and makes `cachewritespeed`/`cachereadspeed` unimposable (tmpfs has no block device); `false` uses disk-backed volumes, which may cap throughput |
| `-fuse`       | `true`/`false`              | `true`        | `true` grants `/dev/fuse` + `SYS_ADMIN` and mounts `/app/mnt`; `false` drops the `fuse-*` tests                                                                                                                          |
| `-sets`       | `all-at-once`, `sequential` | `all-at-once` | `sequential` posts one fixture group at a time to a fresh server, for hosts too small for all of them; rules out the baselines and the concurrent test                                                                   |
| `-size-mb`    | integer MiB, `0` = 256      | `0`           | payload size per fixture                                                                                                                                                                                                 |
| `-build`      | `true`/`false`              | `false`       | `docker compose build` first                                                                                                                                                                                             |
| `-keep`       | `true`/`false`              | `false`       | leave the stack up (skip `down -v`)                                                                                                                                                                                      |
| `-probe-only` | `true`/`false`              | `false`       | print what this host can impose, then exit                                                                                                                                                                               |

What is measured:

| Flag         | Options                                     | Default          | What                                                             |
| ------------ | ------------------------------------------- | ---------------- | ---------------------------------------------------------------- |
| `-tests`     | test names, comma list, globs               | all enabled      | which read tests to run (`webdav-*`, `*-rar-*`)                  |
| `-matrix`    | `key=v1,v2` tokens, space separated         | harness defaults | rig axes to sweep, see below                                     |
| `-app-env`   | `NAME=v1,v2` tokens, space separated        | product defaults | app env axes to sweep; an empty value omits the variable         |
| `-combine`   | `cartesian`, `pairwise`                     | `cartesian`      | every combination, or a greedy covering array over every 2-tuple |
| `-phases`    | `cold`, `warm`, comma list                  | `cold,warm`      | cold restarts with an empty cache, warm reads it again           |
| `-repeats`   | integer                                     | `3`              | readings per (test, phase, cell)                                 |
| `-baselines` | `news`, `cache-write`, `cache-read`, `none` | all three        | which rig baselines to run                                       |

Test output data:

| Flag                 | Options          | Default                            | What                                   |
| -------------------- | ---------------- | ---------------------------------- | -------------------------------------- |
| `-out`               | path             | `test/build/results.csv`           | raw reading CSV                        |
| `-summary`           | path, `disable`  | `test/build/summary.csv`           | summary CSV; `disable` writes only raw |
| `-baselines-out`     | path             | `test/build/baselines.csv`         | raw baseline CSV                       |
| `-baselines-summary` | path             | `test/build/baselines-summary.csv` | baseline summary CSV                   |
| `-pprof-folder`      | path, `''` = off | `test/build/profiles`              | CPU+heap profile per reading           |
| `-pprof-keep`        | `true`/`false`   | `false`                            | keep the raw `.pprof` binaries         |
| `-pprof-top`         | integer          | `5`                                | functions per profile summary row      |

Env: `NNTP_PORT` (1119) and `HTTP_PORT` (8090) select the Newsserver,
`E2E_HOST` (127.0.0.1) is the address to reach the program under test.

### Matrix keys

| Key               | Options                                        | Default      | What                              |
| ----------------- | ---------------------------------------------- | ------------ | --------------------------------- |
| `readtype`        | `sequential`/`seq`, `tail`, `random`, `stride` | `sequential` | the read pattern the client uses  |
| `latency`         | ms                                             | `0`          | netem delay on the news link      |
| `jitter`          | ms                                             | `0`          | netem jitter around that delay    |
| `linespeed`       | MB/s                                           | uncapped     | netem rate on the news link       |
| `linejitter`      | MB/s                                           | `0`          | variation around that rate        |
| `cachewritespeed` | MB/s                                           | uncapped     | docker blkio on the cache device  |
| `cachereadspeed`  | MB/s                                           | uncapped     | docker blkio on the cache device  |
| `seed`            | integer                                        | `1`          | seed for the random read patterns |

```sh
go run ./test/run \
  -matrix 'readtype=sequential,random latency=0,50 linespeed=100' \
  -app-env 'NNTP_PIPELINE_SIZE=,4,16 READAHEAD_CHUNK=1M,8M' \
  -combine pairwise
```

An unswept key takes its default above, which is "no impediment".
`harness.RigDefaults` pins `LOGLEVEL=WARN`
and a 2 GiB `CACHE_MAX_SIZE` on every cell that does not sweep them, since an
unbounded cache fills the volume and per-read logging is measurable work. Nothing
else is set, so a run with no axes measures the product as it ships.

## Baselines

`-baselines` measures the rig itself, so a reading can be told apart from the
ceiling it ran into: the news link, the cache device and the host under this
cell are what a test can at best reach, and a result at that number is the rig
rather than the product.  
Its the best case for a test.

`go run ./test/baseline` runs the news one on its own, against a rig brought up
with `-keep`:

| Flag             | Default                    | What                                                 |
| ---------------- | -------------------------- | ---------------------------------------------------- |
| `-nzb`           | `test/build/nzb/plain.nzb` | nzb to take message-ids from                         |
| `-server`        | `127.0.0.1:1119`           | host:port                                            |
| `-user`, `-pass` | `mock`                     | credentials                                          |
| `-max-conn`      | `20`                       | parallel sockets, the app's `USENET_MAX_CONN`        |
| `-pipeline-size` | `32`                       | in flight per socket, the app's `NNTP_PIPELINE_SIZE` |
| `-limit`         | `0`                        | max articles, 0 = all                                |
| `-out`           | stdout line                | CSV out path                                         |

## Output and plotting

Four files in `test/build/`: `results.csv` / `summary.csv` for the reads,
`baselines.csv` / `baselines-summary.csv` for the rig. Raw files hold one row per
reading; summaries one row per (test, phase, cell) with `count`, `failures`,
`measured` (readings a statistic was computed from - a fixture meant to fail
contributes a reading and a failure, never a 0 MiB/s sample), then mean, stddev,
SEM, 95% CI, min/max and percentiles for throughput, wall time, TTFB and iowait,
and the baseline the group was judged against with its ratio and verdict.

`go run ./test/plot` renders a panel per test, a line per remaining axis
combination, each series with its own colour (Okabe-Ito, colour-blind safe), dash
pattern and marker, legend in its own column.

| Flag            | Default                  | What                                            |
| --------------- | ------------------------ | ----------------------------------------------- |
| `-in`           | `test/build/summary.csv` | summary CSV to plot                             |
| `-out`          | `test/build/summary.svg` | SVG to write                                    |
| `-x`            | busiest sweep axis       | column on the x axis                            |
| `-y`            | `mean_mibs`              | column on the y axis                            |
| `-y2`           | none                     | second column, dashed against a right-hand axis |
| `-err`          | `ci95_pm`                | column drawn as an error bar; `''` = none       |
| `-band`         | none                     | two columns `lo,hi` shaded behind each line     |
| `-facet`        | `test`                   | column to give each panel to                    |
| `-logy`         | `false`                  | logarithmic y axis                              |
| `-width`        | `1100`                   | SVG width in px                                 |
| `-panel-height` | `260`                    | panel height in px                              |

## Layout

| Path                    | What                                                                      |
| ----------------------- | ------------------------------------------------------------------------- |
| `run/`                  | the CLI: flags, CSV output, the human table                               |
| `harness/`              | the rig: compose driver, matrix, sweep, read execution, stats             |
| `readclient/`           | read patterns and the per-request timing they report                      |
| `baseline/`, `plot/`    | the two standalone commands                                               |
| `payload/`, `archives/` | fixture generation (`archives` is amd64: RARLAB ships no linux/arm64 rar) |
| `inn/`                  | the news server image and its posting scripts                             |
| `compose*.yaml`         | the base stack plus the disk/ram/fuse overlays                            |

Everything the sweep controls reaches the streamer through `env_file`, never
compose's `environment:`, which wins over the file and would silently pin any
axis listed there.
