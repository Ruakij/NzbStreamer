package sqlstore

import (
	"strings"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

func TestEnsureSourceFilesIsIdempotentAndKeepsRetryAfter(t *testing.T) {
	store := withSourceFiles(t, t.TempDir(), "file.rar")

	if err := store.SetRetryAfter(nzbName, "file.rar", posted.Add(time.Hour)); err != nil {
		t.Fatalf("SetRetryAfter: %v", err)
	}
	// A later ensure, e.g. a re-add or the next build, must not reset what the
	// pass already recorded
	if err := store.EnsureSourceFiles(nzbName, []nzbstore.SourceFile{
		{Filename: "file.rar", PostedAt: posted.Add(2 * time.Hour)},
	}); err != nil {
		t.Fatalf("EnsureSourceFiles again: %v", err)
	}

	var retryAfter, postedAt int64
	if err := store.db.QueryRow(
		"SELECT retry_after, posted_at FROM nzb_source_file WHERE nzb_name = ? AND filename = ?",
		nzbName, "file.rar").Scan(&retryAfter, &postedAt); err != nil {
		t.Fatalf("reading source file row: %v", err)
	}
	if retryAfter != posted.Add(time.Hour).Unix() {
		t.Errorf("retry_after: got %d, want %d", retryAfter, posted.Add(time.Hour).Unix())
	}
	if postedAt != posted.Unix() {
		t.Errorf("posted_at: got %d, want the first written %d", postedAt, posted.Unix())
	}
}

func TestSegmentVerdicts(t *testing.T) {
	store := withSourceFiles(t, t.TempDir(), "file.rar", "other.rar")

	// A fetch measured a size, and a probe confirmed presence
	store.RecordSegmentSize(nzbName, "file.rar", "a@example.com", 1, 716800)
	store.flushSegmentSizes()
	if err := store.RecordProbes(nzbName, []nzbstore.ProbeResult{
		{Filename: "file.rar", Index: 1, MessageID: "a@example.com", Present: true},
	}); err != nil {
		t.Fatalf("RecordProbes: %v", err)
	}
	// A probe answered no, and named the server that did
	if err := store.RecordProbes(nzbName, []nzbstore.ProbeResult{
		{Filename: "file.rar", Index: 2, MessageID: "b@example.com", Present: false, Server: "primary"},
	}); err != nil {
		t.Fatalf("RecordProbes: %v", err)
	}
	// A miss without a server name says nothing durable about any server, but
	// the segment's own answer is still the rotation's verdict
	if err := store.RecordProbes(nzbName, []nzbstore.ProbeResult{
		{Filename: "other.rar", Index: 1, MessageID: "c@example.com", Present: false},
	}); err != nil {
		t.Fatalf("RecordProbes: %v", err)
	}

	verdicts, err := store.SegmentVerdicts(nzbName, []string{"file.rar", "other.rar"})
	if err != nil {
		t.Fatalf("SegmentVerdicts: %v", err)
	}

	fetched := verdicts["file.rar"][1]
	if fetched.MessageID != "a@example.com" || fetched.Size != 716800 ||
		!fetched.Present || fetched.FetchedAt.IsZero() || fetched.CheckedAt.IsZero() || fetched.Fetches != 1 {
		t.Errorf("fetched verdict: got %+v", fetched)
	}
	if fetched.Filename != "file.rar" {
		t.Errorf("verdict filename: got %q", fetched.Filename)
	}

	missing := verdicts["file.rar"][2]
	if missing.Present || missing.Size != -1 || missing.MessageID != "b@example.com" {
		t.Errorf("missing verdict: got %+v, want present=false with size -1", missing)
	}

	// A miss with no server named leaves no segment_missing row, so the
	// rotation's verdict is all there is
	var serverless int
	if err := store.db.QueryRow(
		"SELECT count(*) FROM segment_missing s JOIN nzb_source_file f ON f.id = s.file_id WHERE f.filename = 'other.rar'").Scan(&serverless); err != nil {
		t.Fatalf("counting missing rows: %v", err)
	}
	if serverless != 0 {
		t.Error("a miss without a server wrote a per-server row anyway")
	}
	if verdict := verdicts["other.rar"][1]; verdict.Present || verdict.MessageID != "c@example.com" {
		t.Errorf("serverless miss verdict: got %+v, want present=false", verdict)
	}
	if _, ok := verdicts["gone.rar"]; ok {
		t.Errorf("an unknown filename came back with verdicts: %v", verdicts)
	}

	if _, err := store.SegmentVerdicts("Other.Release.nzb", []string{"file.rar"}); err != nil {
		t.Errorf("verdicts of an unknown nzb: %v", err)
	}
}

// The fetches counter is per segment row, so a re-fetch of one segment of one
// file counts once there and not in the row of the file naming the same post.
func TestFetchesAreCountedPerSegmentRow(t *testing.T) {
	store := withSourceFiles(t, t.TempDir(), "a.rar", "b.rar")

	store.RecordSegmentSize(nzbName, "a.rar", "same@example.com", 1, 700)
	store.flushSegmentSizes()
	store.RecordSegmentSize(nzbName, "a.rar", "same@example.com", 1, 700)
	store.RecordSegmentSize(nzbName, "b.rar", "same@example.com", 1, 700)
	store.flushSegmentSizes()

	verdicts, err := store.SegmentVerdicts(nzbName, []string{"a.rar", "b.rar"})
	if err != nil {
		t.Fatalf("SegmentVerdicts: %v", err)
	}
	if verdicts["a.rar"][1].Fetches != 2 {
		t.Errorf("a.rar fetches: got %d, want 2", verdicts["a.rar"][1].Fetches)
	}
	if verdicts["b.rar"][1].Fetches != 1 {
		t.Errorf("b.rar fetches: got %d, want 1", verdicts["b.rar"][1].Fetches)
	}
}

// A probe the store cannot attach to a source file is an error, since it is
// the evidence a removal acts on.
func TestRecordProbesWithoutSourceFilesIsAnError(t *testing.T) {
	store := storeAt(t, t.TempDir())
	data, err := nzbparser.ParseNzb(strings.NewReader(nzbXML), nzbName)
	if err != nil {
		t.Fatalf("ParseNzb: %v", err)
	}
	if err := store.Add(data, "completed", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err = store.RecordProbes(nzbName, []nzbstore.ProbeResult{
		{Filename: "file.rar", Index: 1, MessageID: "a@example.com", Present: true},
	})
	if err == nil {
		t.Fatal("a probe for an unrecorded file was stored anyway")
	}
}

func TestDeleteCascadesThroughFilesAndSegments(t *testing.T) {
	store := withSourceFiles(t, t.TempDir(), "file.rar")
	if err := store.RecordProbes(nzbName, []nzbstore.ProbeResult{
		{Filename: "file.rar", Index: 1, MessageID: "a@example.com", Present: false, Server: "primary"},
	}); err != nil {
		t.Fatalf("RecordProbes: %v", err)
	}
	store.RecordSegmentSize(nzbName, "file.rar", "a@example.com", 1, 716800)
	store.flushSegmentSizes()

	if err := store.Delete(nzbName); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	for _, query := range []string{
		"SELECT count(*) FROM nzb_source_file",
		"SELECT count(*) FROM segment",
		"SELECT count(*) FROM segment_missing",
	} {
		var count int
		if err := store.db.QueryRow(query).Scan(&count); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if count != 0 {
			t.Errorf("%s left %d rows after the nzb was deleted", query, count)
		}
	}
}

// A verdict that names a server keeps the per-server row, and it is dated.
func TestSegmentMissingRowsCarryTheServer(t *testing.T) {
	store := withSourceFiles(t, t.TempDir(), "file.rar")

	if err := store.RecordProbes(nzbName, []nzbstore.ProbeResult{
		{Filename: "file.rar", Index: 1, MessageID: "a@example.com", Present: false, Server: "primary"},
	}); err != nil {
		t.Fatalf("RecordProbes: %v", err)
	}
	// The same answer again overwrites rather than duplicates
	if err := store.RecordProbes(nzbName, []nzbstore.ProbeResult{
		{Filename: "file.rar", Index: 1, MessageID: "a@example.com", Present: false, Server: "primary"},
	}); err != nil {
		t.Fatalf("RecordProbes again: %v", err)
	}
	if err := store.RecordProbes(nzbName, []nzbstore.ProbeResult{
		{Filename: "file.rar", Index: 1, MessageID: "a@example.com", Present: false, Server: "secondary"},
	}); err != nil {
		t.Fatalf("RecordProbes of another server: %v", err)
	}

	var count, servers int
	if err := store.db.QueryRow("SELECT count(*) FROM segment_missing").Scan(&count); err != nil {
		t.Fatalf("counting missing rows: %v", err)
	}
	if err := store.db.QueryRow(
		"SELECT count(DISTINCT server) FROM segment_missing WHERE checked_at > 0").Scan(&servers); err != nil {
		t.Fatalf("reading servers: %v", err)
	}
	if count != 2 || servers != 2 {
		t.Errorf("segment_missing: got %d rows over %d servers, want 2 over 2", count, servers)
	}

	var present int
	if err := store.db.QueryRow("SELECT present FROM segment WHERE index_ = 1").Scan(&present); err != nil {
		t.Fatalf("reading segment: %v", err)
	}
	if present != 0 {
		t.Errorf("a missing segment reads present=%d", present)
	}
}

func TestSetFilesCarriesTheSourceAndRemoveFilesDropsRows(t *testing.T) {
	// A presented path references the nzb row, so the record has to be there
	store := withSourceFiles(t, t.TempDir(), "file.rar")

	files := []nzbstore.File{
		{Path: "Some.Release/file.rar", Size: 716800, Exact: true, Source: "x.part01.rar"},
		{Path: "Some.Release/Movie.mkv", Size: 1000, Exact: false, Source: "Movie.mkv"},
	}
	if err := store.SetFiles(nzbName, "key", files); err != nil {
		t.Fatalf("SetFiles: %v", err)
	}

	got, err := store.Files(nzbName)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	byPath := make(map[string]nzbstore.File, len(got))
	for _, file := range got {
		byPath[file.Path] = file
	}
	if len(got) != 2 || byPath["Some.Release/file.rar"].Source != "x.part01.rar" ||
		byPath["Some.Release/Movie.mkv"].Source != "Movie.mkv" {
		t.Errorf("files: got %+v, want the sources recorded", got)
	}

	if err := store.RemoveFiles(nzbName, []string{"Some.Release/Movie.mkv", "not-present"}); err != nil {
		t.Fatalf("RemoveFiles: %v", err)
	}
	got, err = store.Files(nzbName)
	if err != nil {
		t.Fatalf("Files after RemoveFiles: %v", err)
	}
	if len(got) != 1 || got[0].Path != "Some.Release/file.rar" {
		t.Errorf("files after RemoveFiles: got %+v", got)
	}

	if err := store.RemoveFiles(nzbName, make([]string, 600)); err != nil {
		t.Fatalf("RemoveFiles beyond one statement: %v", err)
	}
}

// Removing from a record that is not there changes nothing, which is the
// outcome already wanted.
func TestRemoveFilesOfAnUnknownNzb(t *testing.T) {
	store := storeAt(t, t.TempDir())

	if err := store.RemoveFiles("nothing", []string{"x"}); err != nil {
		t.Errorf("RemoveFiles of an unknown nzb: %v", err)
	}
	if files := mustFiles(t, store, "nothing"); len(files) != 0 {
		t.Errorf("files of an unknown nzb: %v", files)
	}
}

func mustFiles(t *testing.T, store *Store, name string) []nzbstore.File {
	t.Helper()
	files, err := store.Files(name)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	return files
}
