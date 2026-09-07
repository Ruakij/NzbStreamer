// Command gen writes the source payloads into the directory given as its only
// argument, for the news server container to archive and post.
package main

import (
	"log"
	"os"

	"git.ruekov.eu/ruakij/nzbStreamer/test/payload"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatalf("usage: %s <dir>", os.Args[0])
	}
	if err := payload.Write(os.Args[1]); err != nil {
		log.Fatal(err)
	}
}
