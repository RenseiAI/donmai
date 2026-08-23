package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: new-controller <registry> <org> <session> <workarea>")
		os.Exit(2)
	}
	registry, err := sessionshim.NewRegistry(os.Args[1])
	if err != nil {
		die(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := sessionshim.Adopt(ctx, sessionshim.AdoptOptions{
		Registry: registry, ControllerID: "v0682-controller",
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
	if controller.SelectedVersion() != shimwire.V1 || controller.SupportsAuthoritativeSnapshot() {
		die(fmt.Errorf("selected=v%d snapshot=%v", controller.SelectedVersion(), controller.SupportsAuthoritativeSnapshot()))
	}
	if _, err := controller.InspectSnapshot(ctx); !errors.Is(err, shimwire.ErrVersionMismatch) {
		die(fmt.Errorf("v1 snapshot refusal=%v", err))
	}
	if err := controller.WriteInput([]byte("released-overlap\r")); err != nil {
		die(err)
	}
	var output strings.Builder
	for {
		select {
		case event := <-controller.Events():
			if event.Kind == sessionshim.EventOutput {
				output.Write(event.Data)
				if strings.Contains(output.String(), "ack:released-overlap") {
					fmt.Printf("PINNED-OVERLAP PASS old=%s selected=1 input_output=ok snapshot_v2=refused\n", "cd71337a87aea7cf0e1e877da3816d06f717e778")
					return
				}
			}
		case <-ctx.Done():
			die(fmt.Errorf("timeout waiting for old shim output: %q", output.String()))
		}
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
