package nzbservice

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbfileanalyzer"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbrecordfactory"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

var (
	ErrAddCancelled    = errors.New("add cancelled")
	ErrNzbStillRunning = errors.New("nzb is still being added")
)

// Stage is how far an add has got. A client api reports it; nothing in the
// service branches on it.
type Stage string

const (
	StageQueued    Stage = "queued"
	StageChecking  Stage = "checking"
	StageBuilding  Stage = "building"
	StageCompleted Stage = "completed"
	StageFailed    Stage = "failed"
	StageCancelled Stage = "cancelled"
	// StageRebuilding is a finished add whose tree is being built again, which
	// is what a settings change costs. The add is over and the item stays
	// history; this says what is happening to the files it already has
	StageRebuilding Stage = "rebuilding"
)

// QueueItem is one add, from accepted to finished. Its id is the nzbs name,
// which is what identifies it everywhere else in the service and what a restart
// derives again from the store, so a client keyed on it survives one.
type QueueItem struct {
	ID string `json:"id"`
	// Category is what the client api that added it called it. Nothing here uses
	// it; a client filters on the value it gave us.
	Category string `json:"category"`
	Stage    Stage  `json:"stage"`
	Bytes    int64  `json:"bytes"`
	// BytesExact says whether Bytes is the size or a lower bound on it
	BytesExact bool `json:"bytes_exact"`
	// Progress is how far the whole add has got, from 0 to 1, and Eta what is
	// left of it in seconds, the wait for a slot included. Both are worked out
	// when the item is handed out rather than kept, since neither is true for
	// longer than the moment it is read; an Eta of 0 is one nothing can estimate
	Progress float64 `json:"progress"`
	Eta      float64 `json:"eta"`
	// Archived is a finished add a client removed from its history. It is a
	// property of the record only: the nzb stays presented and its files stay
	// readable, which is what makes it different from Delete
	Archived bool `json:"archived"`

	Added    time.Time `json:"added"`
	Finished time.Time `json:"finished"`
	Err      string    `json:"error"`

	// Set by Cancel while the add runs, read by the add at its stage
	// boundaries; closed by finish, which is what Cancel waits on
	cancelled bool
	done      chan struct{}

	// When the running stage began, which is what a build is counted down from:
	// an add that waited for a slot spent that wait queued, not building
	stageStarted time.Time
	// Segments the check has probed against the ones it means to probe. The
	// second grows when a file it cannot decide is escalated
	checkDone  int
	checkTotal int
	// What the add costs the news servers, worked out from the nzb before any of
	// it runs: the segments the check plans to probe, and the archive headers the
	// build walks
	probeOps int
	buildOps int
}

// Done reports whether the item belongs in the history rather than the queue.
// A rebuilding one does: its add finished, and what is running is a rebuild of
// what that add produced.
func (i QueueItem) Done() bool {
	return i.Stage == StageCompleted || i.Stage == StageFailed ||
		i.Stage == StageCancelled || i.Stage == StageRebuilding
}

// Add accepts an nzb and returns the id to track it under. The work happens in
// the background, which is the point of the queue: parsing, probing and reading
// an archive header take seconds and a client wants the id now.
func (s *Service) Add(nzbData *nzbparser.NzbData, category string) (string, error) {
	if err := s.enqueue(nzbData, category); err != nil {
		return "", err
	}

	go func() {
		err := s.addNzb(nzbData, true)
		s.finish(nzbData.MetaName, err)
		if err != nil {
			slog.Error("Couldnt add nzb", "MetaName", nzbData.MetaName, "error", err)
		}
	}()

	return nzbData.MetaName, nil
}

// Queue lists the adds still in flight, oldest first.
func (s *Service) Queue() []QueueItem {
	return s.items(false)
}

// History lists the finished adds, oldest first, including the ones restored
// from the store on startup and the archived ones, which carry the flag.
func (s *Service) History() []QueueItem {
	return s.items(true)
}

// Archive takes a finished add out of the default history listing and leaves
// everything it built in place. It is what a client means by removing a
// download: it has imported what it wanted, and the record is only in its way.
// Delete is the one that removes the files.
func (s *Service) Archive(id string, archived bool) error {
	s.queueMutex.Lock()
	item := s.find(id)
	if item == nil {
		s.queueMutex.Unlock()
		return fmt.Errorf("%w: %s", ErrNzbNotFound, id)
	}
	if !item.Done() {
		s.queueMutex.Unlock()
		return fmt.Errorf("%w: %s", ErrNzbStillRunning, id)
	}
	item.Archived = archived
	s.queueMutex.Unlock()

	if err := s.store.SetArchived(id, archived); err != nil {
		return fmt.Errorf("failed recording archived nzb %s: %w", id, err)
	}
	return nil
}

// items lists one side of the queue and works out what each of them has left.
// An add that has not started waits for the ones ahead of it to clear the slots
// they are queueing for, so what is ahead is accumulated over the whole queue
// while the wanted side of it is collected.
func (s *Service) items(done bool) []QueueItem {
	s.mutex.RLock()
	rate := s.rate
	s.mutex.RUnlock()

	perSec := 0.0
	if rate != nil {
		perSec = rate()
	}

	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	slots := s.slots.count()
	ahead := 0.0

	items := make([]QueueItem, 0, len(s.queue))
	for _, item := range s.queue {
		left, total := remainingOps(item, perSec)

		if item.Done() == done {
			copied := *item
			copied.Progress = progressOf(item, left, total)

			// What is ahead only delays an add that has not been handed a slot;
			// unbounded slots mean nothing is waiting for one
			ops := left
			if item.Stage == StageQueued && slots > 0 {
				ops += ahead / float64(slots)
			}
			if perSec > 0 {
				copied.Eta = ops / perSec
			}

			items = append(items, copied)
		}

		if !item.Done() {
			ahead += left
		}
	}
	return items
}

// Cancel takes an add back, wherever it has got to: whatever it built is torn
// down, and the record of it stays, cancelled, because a client that asked is
// owed the answer. Removing that record is what Delete is for.
//
// It blocks until the add is finished. Nothing in flight is interrupted: a
// health check is STATs already issued, and stopping between them would need a
// context through filehealth and the nntp client for the sake of a few
// milliseconds. The add checks at its stage boundaries, so what is waited on is
// at worst the pass that was already running.
func (s *Service) Cancel(id string) error {
	s.queueMutex.Lock()
	item := s.find(id)
	if item == nil {
		s.queueMutex.Unlock()
		return fmt.Errorf("%w: %s", ErrNzbNotFound, id)
	}

	running := !item.Done()
	done := item.done
	item.cancelled = true
	if !running {
		item.Stage = StageCancelled
		item.Finished = time.Now()
	}
	s.queueMutex.Unlock()

	// finish records the stage of one that was still running, since it is what
	// sees the add end
	if running {
		<-done
	}

	s.mutex.Lock()
	nzbData := s.nzbFiledata[id]
	s.unregister(id)
	s.mutex.Unlock()

	if nzbData != nil {
		s.factory.DiscardSegmentStackFromNzbData(nzbData)
	}

	if err := s.store.SetStage(id, string(StageCancelled), ""); err != nil {
		return fmt.Errorf("failed recording cancelled nzb %s: %w", id, err)
	}
	return nil
}

// enqueue records an accepted add, in memory and in the store, so one a restart
// interrupts is resumed and one that fails is still reportable. A name already
// in flight is refused here rather than after the work; a finished one is
// replaced, since an nzb that was removed or that failed may be added again and
// the later attempt is the one worth reporting.
func (s *Service) enqueue(nzbData *nzbparser.NzbData, category string) error {
	// An nzb that is already presented is refused before anything is written,
	// since accepting it would replace the record of the add that built it and
	// then fail on its own duplicate check
	s.mutex.Lock()
	_, present := s.nzbFiledata[nzbData.MetaName]
	s.mutex.Unlock()
	if present {
		return ErrNzbAlreadyExists
	}

	s.queueMutex.Lock()

	if existing := s.find(nzbData.MetaName); existing != nil {
		// A rebuilding one is history and still running, and replacing its
		// record would leave the rebuild writing into an add that replaced it
		if !existing.Done() || existing.Stage == StageRebuilding {
			s.queueMutex.Unlock()
			return ErrNzbAlreadyExists
		}
		s.remove(nzbData.MetaName)
	}

	bytes, bytesExact := totalBytes(nzbData)
	probeOps, buildOps := s.plannedOps(nzbData)
	s.queue = append(s.queue, &QueueItem{
		ID:         nzbData.MetaName,
		Category:   category,
		Stage:      StageQueued,
		Bytes:      bytes,
		BytesExact: bytesExact,
		Added:      time.Now(),
		done:       make(chan struct{}),
		probeOps:   probeOps,
		buildOps:   buildOps,
	})
	s.queueMutex.Unlock()

	// The nzb goes in with it, since what resumes an interrupted add is having
	// the nzb to resume it from
	if err := s.store.Add(nzbData, string(StageQueued), category); err != nil {
		return fmt.Errorf("failed storing nzb %s: %w", nzbData.MetaName, err)
	}

	return nil
}

// plannedOps is what the add will ask the news servers for: the segments the
// check plans to probe, and the reads walking the header of every archive in it
// costs. Both are read off the nzb, without asking the servers anything.
func (s *Service) plannedOps(nzbData *nzbparser.NzbData) (probe, build int) {
	filenames := make([]string, len(nzbData.Files))
	for i := range nzbData.Files {
		filenames[i] = nzbData.Files[i].Filename
	}
	return s.healthChecker.PlannedProbes(nzbData),
		nzbrecordfactory.ArchiveGroups(filenames) * headerWalkOps
}

// restore rebuilds a queue item from what the store kept of an add that ended
// before this process started.
func (s *Service) restore(record nzbstore.Record) {
	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	done := make(chan struct{})
	close(done)

	bytes, bytesExact := totalBytes(record.Data)
	probeOps, buildOps := s.plannedOps(record.Data)
	s.queue = append(s.queue, &QueueItem{
		ID:         record.Data.MetaName,
		probeOps:   probeOps,
		buildOps:   buildOps,
		Category:   record.Category,
		Stage:      Stage(record.Stage),
		Bytes:      bytes,
		BytesExact: bytesExact,
		Archived:   record.Archived,
		Added:      record.AddedAt,
		Finished:   record.FinishedAt,
		Err:        record.Err,
		done:       done,
	})
}

// rebuilding moves a restored item into and back out of StageRebuilding. Only
// a completed add is rebuilt, so that is where it goes back to - unless the
// rebuild failed, which recorded its own stage and is left alone. It is not
// written to the store: a rebuild a restart interrupts has to happen again, and
// what says so is the completed record it started from.
func (s *Service) rebuilding(id string, building bool) {
	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	item := s.find(id)
	switch {
	case item == nil:
	case building:
		item.Stage = StageRebuilding
	case item.Stage == StageRebuilding:
		item.Stage = StageCompleted
	}
}

// failedRebuild records a tree that could not be built again. A completed
// download whose files nothing can reach is not a completed one, so it says so
// here and in the store.
func (s *Service) failedRebuild(id string, err error) {
	s.queueMutex.Lock()
	item := s.find(id)
	if item == nil {
		s.queueMutex.Unlock()
		return
	}
	item.Stage = StageFailed
	item.Err = err.Error()
	message := item.Err
	s.queueMutex.Unlock()

	if err := s.store.SetStage(id, string(StageFailed), message); err != nil {
		slog.Error("Failed recording a rebuild that failed", "MetaName", id, "error", err)
	}
}

// stage moves an item along and reports whether it may go on. Restoring the
// store calls the same add path without an item, so an unknown id carries on.
func (s *Service) stage(id string, stage Stage) error {
	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	item := s.find(id)
	// Restoring the store walks the same add path over a record that already
	// ended, and an unknown id is one nothing is tracking; neither has a stage
	// left to move
	if item == nil || item.Done() {
		return nil
	}
	if item.cancelled {
		return fmt.Errorf("%w: %s", ErrAddCancelled, id)
	}

	item.Stage = stage
	item.stageStarted = time.Now()
	return nil
}

// progress records how far the check of an add has got. It is called once per
// probed segment, so it is kept to what a lock and two writes cost; nothing
// about it is written to the store, since an add a restart interrupts starts
// its stage again.
func (s *Service) progress(id string, done, total int) {
	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	if item := s.find(id); item != nil {
		item.checkDone, item.checkTotal = done, total
	}
}

// finish records how an add ended, in the store as well, and releases whoever is
// waiting on it.
func (s *Service) finish(id string, err error) {
	s.queueMutex.Lock()

	item := s.find(id)
	if item == nil {
		s.queueMutex.Unlock()
		return
	}

	switch {
	case item.cancelled:
		item.Stage = StageCancelled
	case err != nil:
		item.Stage = StageFailed
		item.Err = err.Error()
	default:
		item.Stage = StageCompleted
	}
	item.Finished = time.Now()
	stage, message := item.Stage, item.Err
	close(item.done)

	s.queueMutex.Unlock()

	if err := s.store.SetStage(id, string(stage), message); err != nil {
		slog.Error("Failed recording how an add ended", "MetaName", id, "stage", stage, "error", err)
	}
}

// find and remove walk the queue; the caller holds queueMutex.
func (s *Service) find(id string) *QueueItem {
	for _, item := range s.queue {
		if item.ID == id {
			return item
		}
	}
	return nil
}

func (s *Service) remove(id string) {
	for i, item := range s.queue {
		if item.ID == id {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return
		}
	}
}

// totalBytes is what the nzb presents decoded, which is not what its bytes-hints
// add up to wherever the producer counted wire bytes. exact is false where a
// segment of it could only be estimated, which is the last one of every file.
func totalBytes(nzbData *nzbparser.NzbData) (bytes int64, exact bool) {
	sizer := nzbfileanalyzer.NewSegmentSizer(nzbData)

	exact = true
	for i := range nzbData.Files {
		for _, segment := range nzbData.Files[i].Segments {
			size, segmentExact := sizer.Size(segment.BytesHint)
			bytes += int64(size)
			exact = exact && segmentExact
		}
	}
	return bytes, exact
}
