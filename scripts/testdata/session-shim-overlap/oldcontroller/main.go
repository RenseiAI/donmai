package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

func main() {
	if len(os.Args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: old-controller <registry> <org> <session> <workarea> <released-sha>")
		os.Exit(2)
	}
	registry, err := sessionshim.NewRegistry(os.Args[1])
	if err != nil {
		die(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := sessionshim.Adopt(ctx, sessionshim.AdoptOptions{
		Registry: registry, ControllerID: "released-v0682-controller",
		ExpectedWorkarea: func(sessionshim.Identity) string { return os.Args[4] },
	})
	if err != nil {
		die(err)
	}
	defer result.Close()
	if len(result.Adopted) != 1 {
		die(fmt.Errorf("adopted=%d quarantined=%d", len(result.Adopted), len(result.Quarantined)))
	}
	controller := result.Adopted[0]
	if controller.SelectedVersion() != shimwire.V2 {
		die(fmt.Errorf("selected=v%d want=v2", controller.SelectedVersion()))
	}
	if snapshot, err := controller.InspectSnapshot(ctx); err != nil || len(snapshot.Bytes) == 0 {
		die(fmt.Errorf("released controller v2 snapshot=%+v err=%v", snapshot, err))
	}
	if err := controller.WriteInput([]byte("new-shim-old-controller\r")); err != nil {
		die(err)
	}
	var output strings.Builder
	for {
		select {
		case event := <-controller.Events():
			if event.Kind == sessionshim.EventOutput {
				output.Write(event.Data)
				if strings.Contains(output.String(), "ack:new-shim-old-controller") {
					if err := controller.Heartbeat(event.Seq); err != nil {
						die(err)
					}
					// Released selected-v2 Heartbeat is write-only. Give the new shim a
					// bounded opportunity to persist its additive sidecar before this old
					// process closes the socket.
					time.Sleep(100 * time.Millisecond)
					fmt.Printf("PINNED-REVERSE-OVERLAP PASS old=%s selected=2 v2_bytes=exact\n", os.Args[5])
					return
				}
			}
		case <-ctx.Done():
			die(fmt.Errorf("timeout waiting for selected-v2 output: %q", output.String()))
		}
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
