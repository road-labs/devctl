// Command billing calls the platform API on a timer and logs the reply. It reads
// the address from PLATFORM_ADDR, which devctl fills from whichever mode the
// platform dependency is in: the peered platform devctl, or the local mock.
package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := os.Getenv("PLATFORM_ADDR")
	log.Printf("billing started, platform at %q", addr)
	for range time.Tick(3 * time.Second) {
		if addr == "" {
			log.Println("billing: no platform address yet — start the platform devctl, or press m then s to use the mock")
			continue
		}
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			log.Printf("billing: platform unreachable at %s: %v", addr, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		log.Printf("billing: platform says: %s", string(body))
	}
}
