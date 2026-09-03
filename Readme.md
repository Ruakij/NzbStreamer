NzbStreamer
===

Presents files described by NZBs from Newsservers on-demand as WebDAV or FUSE with caching & unpacking multi-part-rar and -7z containers.  

<img src="docs/Popeye.png" height="100" width="auto"/>

---

<!-- TOC -->
- [NzbStreamer](#nzbstreamer)
- [1. Description](#1-description)
- [2. Usage](#2-usage)
  - [2.1. How to run](#21-how-to-run)
    - [2.1.1. Docker-compose](#211-docker-compose)
- [3. Problems](#3-problems)
  - [3.1. Segment- and File-sizes](#31-segment--and-file-sizes)
  - [3.2. Archive-Files](#32-archive-files)
- [4. Settings](#4-settings)
- [5. Feature-List](#5-feature-list)
- [6. License](#6-license)
<!-- /TOC -->

# 1. Description

NzbStreamer streams files described by NZBs from Newsservers on-demand via WebDAV or FUSE, with caching and unpacking of multi-part containers like rar and 7z.  
It allows streaming from Usenet without downloading first, using minimal disk space.  
This tool fills the gap left by other tools that are either incompatible or too narrow in scope, aiming to integrate seamlessly with tools like Sonarr and Radarr.

# 2. Usage

On startup, NzbStreamer loads existing NZB files, skipping any with errors. It can start a WebDAV server and/or mount a FUSE filesystem. When a file segment is read, it downloads, assembles, and presents the data. New NZB files added via triggers are parsed, checked, assembled, and made available via the filesystem if they pass plausibility checks.

## 2.1. How to run

### 2.1.1. Docker-compose

Example Compose file with Webdav, Fuse and custom file-blacklist:

```yaml
services:
    nzbstreamer:
        image: nzbstreamer
        volumes:
            - ./cache:/app/.cache
            - ./watch:/app/.watch
            - ./mount:/mount:rshared
        ports:
            - 127.0.0.1:8080:8080
        environment:
            USENET_HOST: your_usenet_host
            USENET_PORT: 563
            USENET_USER: your_usenet_user
            USENET_PASS: your_usenet_pass
            FILESYSTEM_BLACKLIST: "(?i)\\.par2$,(?i)\\.r(ar)?\\d*$,(?i)\\.7z(ip)?\\d*$,(?i)\\.z\\d*$,(?i)\\.zip$,(?i)((^|\\W)(sample|preview)\\W)"
            MOUNT_PATH: /mount
        security_opt:
            - apparmor=unconfined
        cap_add:
            - SYS_ADMIN
        devices:
            - /dev/fuse
```

<br>

Because of the fuse-mount inside the container, several options are required to be set for the mount to work properly:
1. The volume must be mounted with the `rshared` option to allow the propagation of mount events to the host. 
This is required if you want to use the mount on the host or in a different container.
2. apparmor must be disabled for the container to allow the use of FUSE.
3. The container must have the `SYS_ADMIN` capability to allow the use of FUSE.
4. The `/dev/fuse` device must be accessible to the container.

# 3. Problems

## 3.1. Segment- and File-sizes
As the exact size of a segment-data amount isnt known, the program has to rely on the NZB-segment annotation `bytes`, which describes the packed-size of the segment including header and checksum. It is usuall 2-5% larger.

The segment-size is important for determining which segments need to be read next or more importantly where to jump to in case a different part of the file is requested.

The program already compensates for this by checking if the size is close to a known segment-size.  
But this isnt possible in al circumstances, usually for end-segments, and can lead to the file displayed to be slightly smaller than it actually is.

Usually this isnt an issue as the real file will only differ by a few KB, but accessing the file via WebDAV might result in errors when trying to read the whole file as WebDAV expects the file to be static.

This problem doesnt exist, when the file is from within an archive as the archive knows the actual file-size.  
Though this can cause other problems, see below.

## 3.2. Archive-Files
When a file is from within an archive, the program has to unpack the archive to get the file.  

As these are usually compressed in a single stream, the archived-file has to be read from the beginning, until the requested part is read.   This works fine for sequencial reads, but can cause problems when the file is read in a non-sequencial order.  
i.e. Reading a part from end causes the whole archive to be read until the end.

The current implementatiion also doesnt handle seeking backwards, so reading a part early from the current stream causes the whole archive to be read from the beginning again until the part is reached.

If all files within an archive are compressed in a single stream (typically called "Solid") or in seperate ones, depends on the type of archive.  

Specially video-files like mkv are problematic as some metadata required for playback typically resides at the end of the file unless moved to the front. (e.g. Keyframe-index)

# 4. Settings

| Name                              | Default                | Description                                      |
|-----------------------------------|------------------------|--------------------------------------------------|
| **Usenet**
| `USENET_HOST`*                    |                        | Usenet server host                               |
| `USENET_PORT`                     | 563                    | Usenet server port                               |
| `USENET_TLS`                      | true                   | Use TLS for Usenet connection                    |
| `USENET_USER`*                    |                        | Usenet username                                  |
| `USENET_PASS`*                    |                        | Usenet password                                  |
| `USENET_MAX_CONN`                 | 20                     | Maximum Usenet connections to use                |
| `USENET_MAX_ATTEMPTS`             | 3                      | Attempts a request gets before its error is reported |
| `USENET_RETRY_BACKOFF`            | 1s                     | Wait after the first failed attempt, doubled after each further one |
| `USENET_TIMEOUT`                  | 30s                    | Timeout for connecting and for completing a single request |
| `USENET_IDLE_TIMEOUT`             | 2m                     | Time after which an unused connection is closed |
| **Trigger**
| `FOLDER_WATCHER_PATH`             | .watch                 | Watch folder for adding nzbs                     |
| `FOLDER_WATCHER_CONSUME`          | true                   | Delete an nzb file once it has been added; the metadata database keeps it |
| **Presenters**
| `WEBDAV_ADDRESS`                  | :8080                  | Address for WebDAV server; Disabled when unset   |
| `WEBDAV_USERNAME`                 |                        | Username for WebDAV basic auth; Authentication disabled when unset |
| `WEBDAV_PASSWORD`                 |                        | Password for WebDAV basic auth                   |
| `MOUNT_PATH`                      |                        | Path for FUSE mount; Disabled when unset         |
| `MOUNT_OPTIONS`                   |                        | Additional Options for FUSE mount; See mount.fuse3 Manpage for more information |
| **Download-Client-Api**
| `SABNZBD_ADDRESS`                 |                        | Address for the SABnzbd-compatible download client api, e.g. `:8081`; Disabled when unset |
| `SABNZBD_API_KEY`                 |                        | Api key demanded of every request; unauthenticated when unset |
| `SABNZBD_COMPLETE_DIR`            |                        | Path reported to a client as the completed-downloads folder, which is where it imports from; defaults to `MOUNT_PATH` |
| `SABNZBD_CATEGORIES`              | *,tv,movies            | Categories offered to a client; it refuses to save if the one it is configured with is missing |
| **Cache**
| `CACHE_PATH`                      | .cache                 | Path for segment-cache                           |
| `CACHE_MAX_SIZE`                  | 0                      | Maximum cache size in bytes, if unset allows unlimited size (not recommended) |
| **Metadata**
| `METADATA_PATH`                   | .metadata/metadata.db  | Path for the metadata database; WAL puts two sibling files next to it |
| **Prefetch**
| `PREFETCH_TIME`                   | 1s                     | How far ahead of the read position to stay warm, in read-time |
| `PREFETCH_MIN_SEGMENTS`           | 8                      | Segments warmed ahead before a read speed can be measured |
| `PREFETCH_MAX_SEGMENTS`           | 64                     | Upper bound on segments warmed ahead; 0 disables prefetching |
| `PREFETCH_MAX_CONN`               | 0                      | Concurrent prefetches across all files; defaults to `USENET_MAX_CONN` when 0 |
| **Nzb-Options**
| `NZB_FILE_BLACKLIST`              |                        | Early Regex-blacklist, applied after the nzb-file is scanned <br>A file dropped here is not health-checked either, and .par2 dropped here leaves the check without its repair-capacity estimate |
| `NZB_PROBE_SIZE_CONVENTION`       | true                   | Download one segment of an nzb whose segment-size hints do not identify what they count, making its sizes exact; without it they stay estimates until a read has measured them |
| **Health-Probing**
| `PROBE_INITIAL_FILE_PERCENT`      | 0.5                    | Segments checked per content file on the first pass, as a percentage of its segments, spread evenly (so first and last)<br>0 disables checking |
| `PROBE_INITIAL_FILE_MIN_SEGMENTS` | 2                      | Floor on that sample, so a short file is not rounded down to nothing |
| `PROBE_INITIAL_FILE_MAX_SEGMENTS` | 8                      | Cap on that sample, so a huge file does not turn the add into a download |
| `PROBE_EXTENSIVE_FILE_PERCENT`    | 1.0                    | Ceiling on the widened sample a file gets when the first pass cannot decide it; 0 skips the second pass |
| `PROBE_EXTENSIVE_FILE_MAX_SEGMENTS`| 512                   | Absolute cap on that widened sample |
| `PROBE_MAX_MISSING_PERCENT`       | 100                    | Ceiling on accepted damage regardless of par2; 100 lets par2 capacity govern on its own |
| `PROBE_PAR2_SAFETY`               | 0.9                    | Fraction of the estimated par2 capacity to trust, since the capacity is itself estimated |
| `PROBE_UNDECIDED_ACCEPT`          | true                   | Accept a file the second pass still cannot decide |
| `PROBE_CONFIDENCE`                | 0.95                   | Confidence of the interval the verdict is taken from; lower means fewer escalations and more wrong calls |
| `PROBE_PARALLEL`                  | 0                      | Concurrent segment-checks; defaults to `USENET_MAX_CONN` when 0 |
| **Filesystem-Options**
| `FILESYSTEM_BLACKLIST`            | (?i)\.par2$            | Late Regex-blacklist, applied on the actual file added to the filesystem; includes files from archives <br>Can be used to hide archive-files, but leaving unpacked files. Hides .par2 by default, after the health check has counted it |
| `FILESYSTEM_FLATTEN_MAX_DEPTH`    | 1                      | Unpacks files from folders e.g. archives where possible <br>Can be used to hide archive-group-folder |
| `FILESYSTEM_FIX_FILENAME_THRESHOLD`| 0.2                   | Threshold for applying filename-fixing when filename doesnt match nzb meta name |
| **Misc**
| `LOGLEVEL`                        | INFO                   | Logging level, one of {DEBUG, INFO, WARN, ERROR} |

*\* Required*

# 5. Feature-List

-   Triggers
    -   [x] Watch-folder
    -   [ ] SabNzb-API
        -   [ ] Optionally store loaded Nzb in folder
-   Presenters
    -   [x] WebDAV
    -   [x] FUSE
-   Files
    -   Archives
        -   [x] Multipart-Rar
        -   [x] Multipart-7z
        -   [ ] Multipart-Zip
    -   [x] Blacklist
    -   [x] Flatten folders
        -   Needs fixing
    -   [x] Deobfuscate names
    -   [ ] Path templating
-   NZB options
    -   [x] File Blacklist
    -   [ ] Scan segments
        -   [ ] Amount / Percentage
        -   [ ] Unknown sizes
        -   [ ] Periodic rescan
-   Cache
    -   [x] Segment-prefetch
    -   [x] Segment-Cache
        -   [x] Max Size
        -   [ ] Max TTL
    -   [ ] Segment-Metadata-Cache
    -   [ ] Filesystem cache
        -   High-level cache for reduced disk actitivy for compressed archives
-   Internals
    -   [x] Efficient seeking
    -   [ ] Choose efficient Segment-Merger
        -   If we know the size of all Segments, we should use a more efficient merger
    -   [ ] Segment-Merger efficient copying
        -   If we know the size of Segments in a sequence, we should directly write those to out-buffer
    -   [ ] Properly handle Missing articles -> Remove file
        -   Currently only the error is logged
    -   [ ] Nzb Store for more permanent storage
    -   [ ] More efficient opening (and thus reserving) of resources

# 6. License

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published
by the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
