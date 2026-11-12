// Command mockserve runs the scripted Messages API as a standalone process, so the built binary
// can be driven under a pty without an API key.
package main

import (
	"fmt"
	"os"
	"os/signal"

	"github.com/alexbieg/alux/internal/mockapi"
)

func main() {
	s := mockapi.New(mockapi.Turn{Thinking: "pondering this", Text: "All good here."})
	fmt.Println(s.URL)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c
}
