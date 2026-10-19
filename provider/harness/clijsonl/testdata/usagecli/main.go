// usagecli plays a stream-json fixture supplied on stdin through a native CLI
// process. The runner tests pass it as the prompt to the real Claude provider.
package main

import (
	"io"
	"os"
)

func main() {
	if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
		os.Exit(1)
	}
}
