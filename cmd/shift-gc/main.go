package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/gc"
	"shift.dev/shift/internal/securestore"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("shift-gc", config.Version)
		return
	}
	flags := flag.NewFlagSet("shift-gc", flag.ExitOnError)
	stateDir := flags.String("state-dir", "/var/lib/shift", "agent state directory")
	grace := flags.Duration("grace", 24*time.Hour, "minimum orphan age before deletion")
	dryRun := flags.Bool("dry-run", true, "plan only; do not delete")
	planID := flags.String("plan", "", "resume an existing plan instead of generating one")
	_ = flags.Parse(os.Args[1:])

	keys, err := securestore.Open(*stateDir + "/keys")
	fatal(err)
	repository, err := checkpoint.OpenRepository(*stateDir+"/checkpoints", keys)
	fatal(err)
	chunks, err := chunkstore.Open(*stateDir+"/objects", 4<<20, keys)
	fatal(err)
	collector, err := gc.OpenOffline(*stateDir, repository, chunks)
	fatal(err)
	defer collector.Close()
	ctx := context.Background()
	var plan gc.Plan
	if *planID == "" {
		plan, err = collector.Plan(ctx, *grace, time.Now().UTC())
	} else {
		plan, err = collector.Sweep(ctx, *planID, *dryRun)
	}
	fatal(err)
	if *planID == "" && !*dryRun {
		plan, err = collector.Sweep(ctx, plan.ID, false)
		fatal(err)
	}
	encoded, err := json.MarshalIndent(plan, "", "  ")
	fatal(err)
	fmt.Println(string(encoded))
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "shift-gc:", err)
		os.Exit(1)
	}
}
