// Command baseline measures raw INN article-download capacity: C pipelined NNTP
// sockets pull message-ids off one shared queue and discard the bodies,
// counting bytes. The MiB/s it reports is the news server's own capacity, the
// reference the summary's server-bound verdict is judged against. All the
// blitz logic lives in harness.NewsBaseline (deliberately independent raw-NNTP
// sockets and a regex over the nzb, so it cannot share a decoder bug with the
// app); this is a thin CLI over it.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"

	"git.ruekov.eu/ruakij/nzbStreamer/test/harness"
)

func main() {
	nzb := flag.String("nzb", "test/build/nzb/plain.nzb", "nzb to load message-ids from")
	server := flag.String("server", "127.0.0.1:1119", "host:port of the news server")
	user := flag.String("user", "mock", "NNTP username")
	pass := flag.String("pass", "mock", "NNTP password")
	maxConn := flag.Int("max-conn", 20, "parallel NNTP sockets, the app's USENET_MAX_CONN")
	pipelineSize := flag.Int("pipeline-size", harness.DefaultBaselinePipelineSize,
		"ARTICLE requests kept in flight per socket, the app's NNTP_PIPELINE_SIZE")
	limit := flag.Int("limit", 0, "max articles to fetch, 0 = all")
	out := flag.String("out", "", "CSV out path, default a single stdout line")
	flag.Parse()

	host, portStr, err := net.SplitHostPort(*server)
	if err != nil {
		fmt.Fprintf(os.Stderr, "-server: %v\n", err)
		os.Exit(2)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "-server: bad port %q\n", portStr)
		os.Exit(2)
	}

	res, err := harness.NewsBaseline(context.Background(), harness.NewsBaselineOptions{
		Host: host, Port: port, User: *user, Pass: *pass,
		NzbPath: *nzb, MaxConn: *maxConn, PipelineSize: *pipelineSize, Limit: *limit,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "baseline: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("max-conn=%d pipeline-size=%d articles=%d bytes=%d mibs=%.1f failures=%d\n",
		*maxConn, *pipelineSize, res.Articles, res.Bytes, res.MiBs, res.Failures)

	if *out != "" {
		// RFC4180-ish single row: header then one data row, so a script and a
		// human both read it as a minitable.
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create %s: %v\n", *out, err)
			os.Exit(1)
		}
		defer f.Close()
		fmt.Fprintf(f, "max-conn,pipeline-size,articles,bytes,mibs,failures\n")
		fmt.Fprintf(f, "%d,%d,%d,%d,%.1f,%d\n",
			*maxConn, *pipelineSize, res.Articles, res.Bytes, res.MiBs, res.Failures)
	}
}
