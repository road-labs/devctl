// Command mockapi is billing's local stand-in for the platform API. The forward
// mode of the platform dependency runs it on the port devctl allocated, so
// billing can work with nothing else running.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: mockapi <port>")
	}
	addr := ":" + os.Args[1]
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "hello from the MOCK platform (billing's local stand-in)")
	})
	log.Printf("mock platform listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
