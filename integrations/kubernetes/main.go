package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

const finalizer = "shiftgate.dev/migration-finalizer"

func main() {
	config, err := NewConfigFromEnv()
	if err != nil {
		panic(err)
	}
	kube, err := NewKubeClient(config)
	if err != nil {
		panic(err)
	}
	shift, err := NewShiftClient(config)
	if err != nil {
		panic(err)
	}
	controller := &Controller{Kube: kube, Shift: shift, Logger: slog.Default(), PollInterval: config.PollInterval}
	if err := controller.Run(context.Background()); err != nil {
		panic(err)
	}
}

type Controller struct {
	Kube         *KubeClient
	Shift        *ShiftClient
	Logger       *slog.Logger
	PollInterval time.Duration
}

func (c *Controller) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.PollInterval)
	defer ticker.Stop()
	for {
		if err := c.Reconcile(ctx); err != nil {
			c.Logger.Error("reconcile failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Controller) Reconcile(ctx context.Context) error {
	items, err := c.Kube.list(ctx)
	if err != nil {
		return err
	}
	for i := range items {
		if err := c.reconcileOne(ctx, &items[i]); err != nil {
			c.Logger.Error("object reconcile failed", "name", items[i].Metadata.Name, "error", err)
		}
	}
	return nil
}

func (c *Controller) reconcileOne(ctx context.Context, migration *ShiftMigration) error {
	// Deletion must always make progress: a spec edited invalid after the
	// finalizer was added would otherwise be rejected before this branch and
	// stay Terminating forever.
	if migration.Metadata.DeletionTimestamp != nil {
		return c.finalize(ctx, migration)
	}
	if err := validateSpec(migration.Spec); err != nil {
		setStatus(migration, "Rejected", err.Error(), migration.Metadata.Generation, "")
		return c.Kube.update(ctx, *migration)
	}
	if err := c.Kube.addFinalizer(ctx, *migration, finalizer); err != nil {
		return err
	}
	phase, _ := migration.Status["phase"].(string)
	operationID, _ := migration.Status["operationID"].(string)
	if phase == "Completed" || phase == "Failed" || phase == "Rejected" {
		// A phase is terminal only for the generation that produced it. A
		// spec edit after a Rejected/Failed/Completed outcome must reconcile
		// again under the new generation's idempotency key instead of being
		// ignored forever.
		if observedGeneration(migration.Status) == migration.Metadata.Generation {
			return nil
		}
		operationID = ""
	}
	if operationID == "" {
		result, err := c.Shift.createMigration(ctx, migration.Spec, operationKey(migration.Metadata.UID, migration.Metadata.Generation))
		if err != nil {
			setStatus(migration, "Failed", err.Error(), migration.Metadata.Generation, "")
			return c.Kube.update(ctx, *migration)
		}
		operationID = result.ID
		setStatus(migration, "Running", result.Status, migration.Metadata.Generation, operationID)
		return c.Kube.update(ctx, *migration)
	}
	result, err := c.Shift.getMigration(ctx, operationID)
	if err != nil {
		return err
	}
	switch result.Status {
	case "completed":
		setStatus(migration, "Completed", "migration completed", migration.Metadata.Generation, operationID)
	case "failed", "cancelled":
		// Surface why: an empty error_message on a cancelled/failed job must
		// not render as a silent failure.
		reason := result.Error
		if reason == "" {
			if result.Status == "cancelled" {
				reason = "migration cancelled"
			} else {
				reason = "migration failed"
			}
		}
		setStatus(migration, "Failed", reason, migration.Metadata.Generation, operationID)
	default:
		setStatus(migration, "Running", result.Status, migration.Metadata.Generation, operationID)
	}
	return c.Kube.update(ctx, *migration)
}

// finalize settles the in-flight migration before releasing the object. The
// finalizer is removed only after a successful cancel or when the migration
// is verifiably terminal: dropping it earlier leaks an active migration,
// which holds the destination machine's capacity and locks the workload out
// of future migrations.
func (c *Controller) finalize(ctx context.Context, migration *ShiftMigration) error {
	operationID, _ := migration.Status["operationID"].(string)
	if operationID == "" {
		return c.Kube.removeFinalizer(ctx, *migration, finalizer)
	}
	if err := c.Shift.cancelMigration(ctx, operationID, operationKey(migration.Metadata.UID, migration.Metadata.Generation)); err != nil {
		result, lookupErr := c.Shift.getMigration(ctx, operationID)
		if lookupErr != nil || !migrationTerminal(result.Status) {
			return fmt.Errorf("cancel migration %s before finalizing: %w", operationID, err)
		}
	}
	return c.Kube.removeFinalizer(ctx, *migration, finalizer)
}

func validateSpec(spec migrationSpec) error {
	if spec.WorkloadID == "" || spec.SourceMachineID == "" || spec.DestinationMachineID == "" {
		return fmt.Errorf("workloadId, sourceMachineId, and destinationMachineId are required")
	}
	if spec.Mode != "" && spec.Mode != "cold" && spec.Mode != "live" {
		return fmt.Errorf("mode must be cold or live")
	}
	return nil
}

var _ = os.Stdout
