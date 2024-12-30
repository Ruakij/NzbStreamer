AdaptiveMergeReader
===

## Before creating
- Check x segments if their sizeHint and realSize match
    - If they do, use normal reading
    - If they dont, use adaptive reading

#### Normal reading
- Trusts sizeHint for efficient reading from multiple segments into a shared buffer
- Can seek efficiently over multiple segments without having to read them
- When SizeDiff-Error happens
    - Reopen in Adaptive reading mode
    - Seek to currentPosition
    - Execute read

#### Adaptive reading
- Uses sizeHint only as hint for predicting how many segments to read from in parallel
- Seeking requires reading and discarding differences
- Ideally: Once real data is read, sizeHint can be trusted for those segments
    - How to implement in Resource?
        - Maybe via error on Size which can be detected: ErrSizeUnreliable

### Problems
- After seek, when normal reading doesnt detect issues, returned data is of risk to be corrupted
- Adaptive reading cannot return accurate size

## Code
- Start read goroutines: 
    - Until either: expectedTotalRead == len(p)
    - or: minReadBatch is hit (basically in case resources have size 0, this ensures we read from multiple and dont fallback to a pure sequencial approach)
- Once all started
    - Wait for responses (via channel?)
    - On a response, compare expectedRead and actualRead from this reader (result needs readerIndex)
        - On mismatch, recalculate expectedTotalRead
        - If expectedTotalRead < len(p)
            - Start more read goroutines until either: expectedTotalRead == len(p)
            - or: len(P) / actualRead / totalStartedGoroutines
- Once actualTotalRead >= len(p)
    - Seek readers we read too far back
    - Copy buffers in order to p
