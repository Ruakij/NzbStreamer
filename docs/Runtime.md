Runtime
===

## Startup
### Load Nzb-Data from DB

### Place file-entries into VFS

### Start WebDAV


<br>

## New NzbFile
### Scanning
- Parse & check plausability
- Check if segments are available
    - Get headers of posts from Newsserver

### Prepare
- Build Records for files from segments

### Indexing
- Check filetypes
- Archive (7z, rar)
    - List files in archive
- Save in DB

### Presenting
- Add files and archiveFiles to virtual Filesystem
    - `/<nzbFile.header.name>/<filename>`
        - `nzbFile.header.name` may not exist, fallback to base-Filename if not exists

```plantuml
autonumber
hide footbox

footer "NzbStreamer"

title "Read file"

participant User
boundary "File Gateway\ni.e. WebDav" as Gateway
control "Virtual\nFilesystem" as VFS
control "Rar\nstreamer" as Rar
control "Segment cache" as Cache
control "NewsReader" as NewsReader

boundary "NewsServer" as NewsServer

User -> Gateway ++ : Request File
Gateway -> VFS ++ : Open File & Read
VFS -> Rar ++ : Read

Rar -> Rar : Get read position\nin container
Rar -> Cache ++ : Read

Cache -> Cache : Check if segment\nis in cache
alt Cache hit
    Cache ->> Cache : Update accessedAt
else Cache miss
    Cache -> NewsReader ++ : Request Segment
    NewsReader -> NewsServer ++ : Download segment
    NewsServer --> NewsReader -- : CharStream
    NewsReader -> NewsReader : Convert CharStream\nto BinaryStream
    NewsReader --> Cache -- : Data

    Cache -> Cache : Store segment
end

Cache --> Rar -- : Data
Rar -> Rar : Unpack data
Rar --> VFS -- : Data
VFS --> Gateway -- : Data
Gateway --> User -- : Response Data
```
