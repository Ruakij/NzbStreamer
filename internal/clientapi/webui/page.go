package webui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"time"
)

//go:embed static
var static embed.FS

// One etag for every asset: they are embedded in the binary and can only change
// together with it. The zero modtime an embed.FS reports carries no revalidation
// of its own, so without this every load refetches the whole page.
var etag = staticEtag()

func staticEtag() string {
	sum := sha256.New()
	err := fs.WalkDir(static, "static", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		file, err := static.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		if _, err := io.WriteString(sum, path); err != nil {
			return err
		}
		_, err = io.Copy(sum, file)
		return err
	})
	if err != nil {
		panic(err)
	}
	return `"` + hex.EncodeToString(sum.Sum(nil)[:16]) + `"`
}

func staticFiles() http.Handler {
	files := http.FileServerFS(static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		files.ServeHTTP(w, r)
	})
}

func page(w http.ResponseWriter, r *http.Request) {
	index, err := static.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("ETag", etag)
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
}
