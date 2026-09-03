package nzbparser_test

import (
	"bytes"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

// nzb declaring iso-8859-1, with 0xE4 for the "a-umlaut" in its meta name
var latin1Nzb = []byte("<?xml version=\"1.0\" encoding=\"iso-8859-1\" ?>\n" +
	"<nzb>\n" +
	"<head><meta type=\"name\">M\xe4rchen</meta></head>\n" +
	"<file poster=\"p@example.com\" date=\"1700000000\" subject=\"Release &#34;file.rar&#34; yEnc (1/1)\">\n" +
	"<groups><group>alt.binaries.test</group></groups>\n" +
	"<segments><segment bytes=\"100\" number=\"1\">a@example.com</segment></segments>\n" +
	"</file>\n</nzb>")

func TestParseNzbDecodesDeclaredCharset(t *testing.T) {
	nzb, err := nzbparser.ParseNzb(bytes.NewReader(latin1Nzb), "")
	if err != nil {
		t.Fatalf("ParseNzb: %v", err)
	}

	if nzb.MetaName != "Märchen" {
		t.Fatalf("MetaName = %q, want %q", nzb.MetaName, "Märchen")
	}
}
