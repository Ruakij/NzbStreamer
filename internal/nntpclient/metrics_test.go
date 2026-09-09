package nntpclient

import (
	"context"
	"errors"
	"net"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/yenc"
)

// testReader is what every test in this package collects from. The global meter
// provider takes a delegate once per process, so a provider installed per test
// would be dropped and its reader would stay empty; the tests name their server
// instead and read the series that carries it.
var testReader = sdkmetric.NewManualReader()

// testServer is the name this file's fetch runs under, so the other tests that
// dial loopback do not land in the same series.
const testServer = "metrics-test"

func TestMain(m *testing.M) {
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testReader)))
	os.Exit(m.Run())
}

// samples counts what one instrument recorded for one server, keyed by the
// attribute the test is about. Histograms carry their own count, which is what
// makes a total command count out of a latency.
func samples(t *testing.T, server, instrument string, key attribute.Key) map[string]uint64 {
	t.Helper()

	counts := map[string]uint64{}
	for _, point := range collect[metricdata.Histogram[float64]](t, instrument).DataPoints {
		if name, _ := point.Attributes.Value(serverKey); name.AsString() != server {
			continue
		}
		value, _ := point.Attributes.Value(key)
		counts[value.AsString()] += point.Count
	}
	return counts
}

// collect reads one instrument out of a scrape, as the data it holds.
func collect[D metricdata.Aggregation](t *testing.T, instrument string) D {
	t.Helper()

	var collected metricdata.ResourceMetrics
	if err := testReader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect: %v", err)
	}

	var data D
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != instrument {
				continue
			}
			typed, ok := m.Data.(D)
			if !ok {
				t.Fatalf("%s is a %T, want a %T", instrument, m.Data, data)
			}
			return typed
		}
	}
	return data
}

// The instruments are only worth having if a fetch fills them: one segment over
// a pipelined connection is a tcp connect, a greeting, a GROUP and an ARTICLE,
// each timed apart.
func TestFetchRecordsWhatItSpokeAndWhenItConnected(t *testing.T) {
	s := newFakeNNTP(t)
	host, port, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		t.Fatalf("addr: %v", err)
	}
	number, _ := strconv.Atoi(port)

	// dialed for real, so the connect phases are the ones a server answers
	c := New(Config{Host: host, Name: testServer, Port: number, MaxConns: 2, ConnectionPipeliningSize: 4, Attempts: 1})

	f := newFetches(t, c)
	f.start("a")

	fc := s.conn()
	fc.expect("GROUP " + testGroup)
	fc.groupOK(testGroup)
	fc.article()
	fc.respond([]byte("body-a"))

	f.wait()
	f.assertBody("a", "body-a")

	responses := samples(t, testServer, "nntp.response.latency", responseKey)
	for _, response := range []string{responseGreeting, responseGroup, responseArticle} {
		if responses[response] != 1 {
			t.Errorf("%s latency has %d samples, want 1", response, responses[response])
		}
	}

	// plaintext, so tls is the phase that does not happen
	connects := samples(t, testServer, "nntp.socket.connect.latency", phaseKey)
	if connects[phaseTCP] != 1 {
		t.Errorf("tcp latency has %d samples, want 1", connects[phaseTCP])
	}
}

// gauges reads what one async instrument observed, per server.
func gauges(t *testing.T, instrument string) map[string]int64 {
	t.Helper()

	values := map[string]int64{}
	for _, point := range collect[metricdata.Gauge[int64]](t, instrument).DataPoints {
		name, _ := point.Attributes.Value(serverKey)
		values[name.AsString()] = point.Value
	}
	return values
}

// A quota is only worth graphing against its allowance, and a period that has
// run out has to read as spent-nothing rather than as still full, since that is
// what the next fetch makes of it.
func TestQuotaIsObservedAgainstItsAllowance(t *testing.T) {
	NewPool([]ServerConfig{
		{Server: &fakeServer{}, Name: "live", QuotaBytes: 1000, QuotaPeriod: time.Hour},
	}, &fakeQuotaStore{used: 400, start: time.Now()}, BreakerConfig{})
	NewPool([]ServerConfig{
		{Server: &fakeServer{}, Name: "rolled", QuotaBytes: 1000, QuotaPeriod: time.Hour},
		{Server: &fakeServer{}, Name: "unmetered"},
	}, &fakeQuotaStore{used: 900, start: time.Now().Add(-2 * time.Hour)}, BreakerConfig{})

	used := gauges(t, "nntp.quota.used")
	limit := gauges(t, "nntp.quota.limit")

	if used["live"] != 400 || limit["live"] != 1000 {
		t.Errorf("a live period reads %d of %d; want 400 of 1000", used["live"], limit["live"])
	}
	if used["rolled"] != 0 {
		t.Errorf("a period that ended reads %d used; want 0", used["rolled"])
	}
	if limit["unmetered"] != 0 {
		t.Errorf("an unmetered server reads a limit of %d; want 0", limit["unmetered"])
	}
}

// The connection gauges are the pair a queueing request is read against, so
// they have to be there whether or not the client pipelines.
func TestConnectionsAreObservedAgainstTheirLimit(t *testing.T) {
	New(Config{Host: "plain", MaxConns: 1})
	New(Config{Host: "piped", MaxConns: 6, ConnectionPipeliningSize: 4})

	limit := gauges(t, "nntp.connections.limit")
	if limit["plain"] != 1 || limit["piped"] != 6 {
		t.Errorf("limits read %d and %d; want 1 and 6", limit["plain"], limit["piped"])
	}
	if open := gauges(t, "nntp.connections.open"); open["piped"] != 0 {
		t.Errorf("a client that dialled nothing has %d connections open", open["piped"])
	}
	// a client that does not pipeline reports no window at all
	window := gauges(t, "nntp.pipeline.window")
	if _, ok := window["plain"]; ok || window["piped"] != 4 {
		t.Errorf("the windows read %v; want the pipelined client's 4 and nothing for the plain one", window)
	}
}

// The socket counters come out of the kernel through a getsockopt whose option
// and struct differ per platform, so what this asserts is that the one built
// here reads a live connection at all: a wrong option answers an error and the
// metrics would be silently absent.
func TestSocketStatsReadsALiveConnection(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("no TCP_INFO on %s", runtime.GOOS)
	}

	client, _ := loopback(t)
	if _, err := client.Write([]byte("something to count\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	info, ok := socketStats(client)
	if !ok {
		t.Fatal("the socket counters did not read")
	}
	if info.packets == 0 {
		t.Errorf("a connection that sent something counted %d packets", info.packets)
	}

	client.Close()
	if _, ok := socketStats(client); ok {
		t.Error("a closed connection still answered, so a sample can be taken too late")
	}
}

// The error kinds are what an operator sorts by, so a truncated body has to
// stay apart from a refused login however deeply either is wrapped.
func TestErrorKindNamesTheFailure(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{ErrAuthFailed, "auth"},
		{ErrTooManyConnections, "conns_exceeded"},
		{staleIf(true, errors.New("reset")), "stale"},
		{errors.Join(errors.New("decoding"), yenc.ErrTruncated), "decode"},
		{ErrUnexpectedResponse, "protocol"},
		{&net.OpError{Op: "dial", Err: errors.New("refused")}, "connect"},
		{errors.New("something else"), "other"},
	} {
		if got := errorKind(test.err); got != test.want {
			t.Errorf("errorKind(%v) = %q, want %q", test.err, got, test.want)
		}
	}
}
