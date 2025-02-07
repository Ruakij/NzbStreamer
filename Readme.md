# NzbStreamer

Presents files described by NZBs from Newsservers on-demand as WebDAV or FUSE with caching & unpacking multi-part-rar and -7z containers.


## Features

-   Triggers
    -   [x] Blackhole-folder
    -   [ ] SabNzb-API
        -   [x] Optionally store loaded Nzb in folder
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
    -   [x] Readahead cache
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
