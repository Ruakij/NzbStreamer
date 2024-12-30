## Settings
### Newsserver
- Host
- Port
- SSL
- User
- Pass
- MaxConnections

### Cache
- Location
    - `<workdir>/.cache`
- MaxSize
- MaxTLL

### Nzb
- RescanInterval
- ScanIgnoreSegmentMissing
    - `false`
- ScanArchiveFileList
    - `true`

### Filesystem
- FilepathTemplate
    - `/%nzbFileName%/%filename%`
    - Possible: [`nzbFileName`, `filename`, `archiveName`]
- IgnoreFilesWithExtensions
    - `par2`
- ReplaceBaseFilenameWithNzbBelowFuzzyThreshold
    - `0.4`
- Adaptive Sequential Readahead Cache
    - To enhance sequencial read-performance system tries to guess how much is read next by calculating `readaheadAmount = (AvgSpeed in AdaptiveReadaheadCacheAvgSpeedTime) * AdaptiveReadaheadCacheTime`
    - AdaptiveReadaheadCacheAvgSpeedTime
        - `1s`
        - Over how much time average speed is calculated
    - AdaptiveReadaheadCacheTime
        - `1s`
        - How far ahead to read in time
    - AdaptiveReadaheadCacheMinSize
        - `0`
        - Minimum amount of readahead bytes
    - AdaptiveReadaheadCacheMaxSize
        - `67108864`
        - Maximum amount of readahead bytes
    