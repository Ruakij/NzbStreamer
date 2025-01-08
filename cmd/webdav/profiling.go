package main

import (
	"fmt"
	"net/http"

	"github.com/arl/statsviz"

	_ "net/http/pprof"
)

func init() {
	initPprof()
	initStatsviz()
}

func initPprof() {
	go func() {
		http.ListenAndServe("localhost:6060", nil)
	}()
}

func initStatsviz() {
	mux := http.NewServeMux()
	_ = statsviz.Register(mux,
		statsviz.Root(""),
	)

	go func() {
		fmt.Println(http.ListenAndServe("localhost:8081", mux))
	}()
}
