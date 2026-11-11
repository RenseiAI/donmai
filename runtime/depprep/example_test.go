package depprep_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/RenseiAI/donmai/runtime/depprep"
)

// Example shows a non-agent executor preparing a fresh checkout with no
// keeper, no confiner and no registry credentials. It uses only this
// package's exported types, as a caller in another module must.
func Example() {
	entry := depprep.Entry{
		Manager:        "pnpm",
		Ecosystem:      "node",
		Versions:       ">=9 <10",
		KitID:          "typescript",
		Lockfiles:      []string{"pnpm-lock.yaml"},
		VersionCommand: "pnpm --version",
		Commands: depprep.Commands{
			Fetch:   "pnpm fetch",
			Install: "pnpm install --offline --frozen-lockfile",
		},
		Store:     depprep.StoreLayout{Env: []string{"NPM_CONFIG_STORE_DIR"}, Sharing: "content-addressed", Integrity: "on-use"},
		ImportEnv: map[string]string{"NPM_CONFIG_PACKAGE_IMPORT_METHOD": "clone-or-copy"},
		NoCode: &depprep.NoCode{Env: map[string]string{
			"npm_config_ignore_scripts":  "true",
			"npm_config_ignore_pnpmfile": "true",
		}},
		Installed: []string{"node_modules", "**/node_modules"},
	}
	preparer, err := depprep.New(depprep.Config{Entries: depprep.StaticEntries{entry}})
	if err != nil {
		fmt.Println(err)
		return
	}

	checkout, _ := os.MkdirTemp("", "checkout-")
	scratch, _ := os.MkdirTemp("", "scratch-")
	defer func() { _ = os.RemoveAll(checkout); _ = os.RemoveAll(scratch) }()

	// A seven-minute budget less a three-minute clone leaves four.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	report, err := preparer.Prepare(ctx, depprep.Request{
		Root:      checkout,
		Subject:   depprep.Subject{Kind: depprep.SubjectInvocation, ID: "invocation-1"},
		Toolchain: nil,
		Scratch:   scratch,
		Network:   depprep.NetworkAllow,
	})
	switch {
	case errors.Is(err, depprep.ErrDeadlineExceeded):
		fmt.Println("retry later")
	case err != nil:
		fmt.Println("refuse:", depprep.CodeOf(err))
	default:
		// The empty checkout holds no pnpm-lock.yaml, so no entry selects it.
		fmt.Println(report.Outcome, report.MissReason)
	}
	// Output: not_applicable no_entry
}
