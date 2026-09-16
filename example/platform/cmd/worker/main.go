// Command worker is a service with no ports: it just does work on a timer, the
// way a queue consumer or a scheduler would.
package main

import (
	"log"
	"time"
)

func main() {
	log.Println("platform worker started")
	for range time.Tick(3 * time.Second) {
		log.Println("platform worker: did a unit of work")
	}
}
