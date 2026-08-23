package main

import (
	"fmt"
	"os"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
)

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: new-shim <registry> <org> <session> <workarea>")
		os.Exit(2)
	}
	registry, err := sessionshim.NewRegistry(os.Args[1])
	if err != nil {
		die(err)
	}
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity:     sessionshim.Identity{OrgID: os.Args[2], SessionID: os.Args[3]},
		Registry:     registry,
		Spec:         ptyhost.Spec{Command: []string{"/bin/sh", "-c", `while IFS= read -r line; do printf 'ack:%s\n' "$line"; done`}},
		WorkareaPath: os.Args[4],
		ProcessEpoch: 1,
	})
	if err != nil {
		die(err)
	}
	<-shim.Done()
}

func die(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
