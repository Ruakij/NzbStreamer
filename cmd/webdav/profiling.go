package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"

	"github.com/arl/statsviz"
)

func init() {
	initPprof()
	initStatsviz()
}

func initPprof() {
	go func() {
		err := http.ListenAndServe("localhost:6060", nil)
		if err != nil {
			panic(err)
		}
	}()
}

func initStatsviz() {
	mux := http.NewServeMux()
	err := statsviz.Register(mux,
		statsviz.Root(""),
	)
	if err != nil {
		panic(err)
	}

	go func() {
		fmt.Println(http.ListenAndServe("localhost:8081", mux))
	}()
}
