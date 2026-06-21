// Command clockprinter prints the current time once per second. Used as an injection target during local testing. (phase 6)
package main

import (
	"fmt"
	"time"
)

func main() {
	for {
		fmt.Println(time.Now().Format(time.RFC3339))
		time.Sleep(time.Second)
	}
}
