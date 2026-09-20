// Command hermes-egress-guard — the single Go binary behind both egress-guard
// services (plan Units 3.2/3.3, Go migration).
//
// One image, two roles, selected by the FIRST argv (the Deployment passes
// `authorizer` or `reaper` as argv[1], mirroring how the Python image selected
// the module path `egress_guard.authorizer` / `egress_guard.reaper`). Both
// roles share the whole guard package but configure very differently:
//
//   authorizer — the Envoy ext_authz decision point on :8080 (/check).
//   reaper     — consumes the authorizer's signed events on :8080 (/events),
//                quarantines sessions, sweeps the ledger (2.E).
//
// A missing/unknown role exits loudly (2) instead of silently serving a wrong
// role: the Deployment's CrashLoopBackOff is the misconfiguration signal.
// The library lives in the root `package guard` (siblings), imported here by
// the module path hermes-egress-guard; this package-main file is the entrypoint.
package main

import (
	"log"
	"os"

	"hermes-egress-guard"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: hermes-egress-guard <authorizer|reaper>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "authorizer":
		if 0 != guard.AuthorizerMain(os.Args) {
			os.Exit(1)
		}
	case "reaper":
		if 0 != guard.ReaperMain(os.Args) {
			os.Exit(1)
		}
	default:
		log.Fatalf("hermes-egress-guard: unknown role %q (expected authorizer|reaper)", os.Args[1])
		os.Exit(2)
	}
}
