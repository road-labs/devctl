// Command api is the platform's HTTP service. It binds the address devctl hands
// it in HTTP_ADDR and answers every request with a line naming that address, so
// a caller can see which port it actually reached.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8100"
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from the platform api, listening on %s\n", addr)
	})
	log.Printf("platform api listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
