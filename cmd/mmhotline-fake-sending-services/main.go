// Command mmhotline-fake-sending-services runs the fake outside services for local
// development and the Linux test suites. Point MMHotline at it with
// MMHOTLINE_FAKE_SENDING_SERVICES_URL. It is never part of the mmhotline binary.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"mmhotline/internal/fakesendingservices"
)

func main() {
	listen := flag.String("listen-address", "127.0.0.1:8090", "address to listen on")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: mmhotline-fake-sending-services [--listen-address 127.0.0.1:8090]")
		os.Exit(4)
	}
	server := &http.Server{Addr: *listen, Handler: fakesendingservices.New(), ReadHeaderTimeout: 5 * time.Second}
	fmt.Printf("Fake sending services listening on http://%s/ (nothing is really sent)\n", *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "mmhotline-fake-sending-services: %s\n", err)
		os.Exit(1)
	}
}
