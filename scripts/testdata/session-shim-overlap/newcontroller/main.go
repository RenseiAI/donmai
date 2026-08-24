package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

func main() {
	if len(os.Args) != 7 {
		fmt.Fprintln(os.Stderr, "usage: new-controller <registry> <org> <session> <workarea> <selected-version> <released-sha>")
		os.Exit(2)
	}
	wantCapabilities := strings.Join([]string{
		daemon.SessionShimCapabilityAuthoritativeSnapshotV2,
		daemon.SessionShimCapabilityCarrierEpochPrepareCommit,
		daemon.SessionShimCapabilityDurableCarrierProofV2,
		daemon.SessionShimCapabilityFullHostFrameV3,
		daemon.SessionShimCapabilityInteractiveAttachV2,
	}, ",")
	if got := strings.Join(daemon.RequiredSessionShimHostCapabilities(), ","); got != wantCapabilities ||
		strings.Contains(got, daemon.SessionShimCapabilityDurableCarrierProofV1) {
		die(fmt.Errorf("new-controller capability tuple=%q want=%q", got, wantCapabilities))
	}
	wantVersion, err := strconv.ParseUint(os.Args[5], 10, 32)
	if err != nil {
		die(err)
	}
	registry, err := sessionshim.NewRegistry(os.Args[1])
	if err != nil {
		die(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := sessionshim.Adopt(ctx, sessionshim.AdoptOptions{
		Registry: registry, ControllerID: "v0683-full-frame-controller",
		ExpectedWorkarea:      func(sessionshim.Identity) string { return os.Args[4] },
		RequireFullHostFrames: true,
	})
	if err != nil {
		die(err)
	}
	defer result.Close()
	if len(result.Adopted) != 1 {
		die(fmt.Errorf("adopted=%d quarantined=%d", len(result.Adopted), len(result.Quarantined)))
	}
	controller := result.Adopted[0]
	if controller.SelectedVersion() != uint32(wantVersion) {
		die(fmt.Errorf("selected=v%d want=v%d", controller.SelectedVersion(), wantVersion))
	}
	snapshotResult := "available"
	if wantVersion == uint64(shimwire.V1) {
		snapshotResult = "refused"
		if controller.SupportsAuthoritativeSnapshot() {
			die(errors.New("selected v1 unexpectedly advertises authoritative snapshot"))
		}
		if _, err := controller.InspectSnapshot(ctx); !errors.Is(err, shimwire.ErrVersionMismatch) {
			die(fmt.Errorf("v1 snapshot refusal=%v", err))
		}
	} else {
		if !controller.SupportsAuthoritativeSnapshot() {
			die(errors.New("selected v2 omitted authoritative snapshot"))
		}
		if snapshot, err := controller.InspectSnapshot(ctx); err != nil || len(snapshot.Bytes) == 0 {
			die(fmt.Errorf("v2 snapshot=%+v err=%v", snapshot, err))
		}
		if controller.SupportsFullHostFrames() {
			die(errors.New("released max-2 shim selected v3 full-frame rail"))
		}
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
					fmt.Printf("PINNED-OVERLAP PASS old=%s selected=%d input_output=ok snapshot_v2=%s proof_v2=exact\n", os.Args[6], wantVersion, snapshotResult)
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
