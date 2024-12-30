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

```
