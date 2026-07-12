package attachtest

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// Main is the subprocess entry point for the stub relay, so a test binary can
// re-exec itself to run the relay in a separate process (the kill -9 end-to-end
// test SIGKILLs it and restarts on the same port). It blocks until killed or
// interrupted. It prints the bound address as the first stdout line so the
// parent can wait for readiness.
func Main(args []string) int {
	fs := flag.NewFlagSet("attachtest-relay", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:0", "loopback listen address")
	room := fs.String("room", "room-1", "room id")
	ring := fs.Int("ring", 256, "ring size (frames)")
	refuse := fs.Bool("refuse-wss", false, "refuse WSS upgrades (force degraded)")
	drop := fs.Bool("drop-post-once", false, "drop the first applied host POST response")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	s := New(Config{
		Addr:             *addr,
		RoomID:           *room,
		RingSize:         *ring,
		RefuseWSS:        *refuse,
		DropHostPOSTOnce: *drop,
	})
	if err := s.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "attachtest relay:", err)
		return 1
	}
	// First stdout line = readiness signal + bound address.
	fmt.Println(s.Addr())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	_ = s.Close()
	return 0
}
