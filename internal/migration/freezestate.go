package migration

import (
	"time"
)

// migrationFreeze is the durable account of one migration's relationship to
// its workload's freeze. It lives beside the migration record instead of on
// it because model.Migration is owned elsewhere and must not grow fields for
// one package's recovery rules. The facts are written before the freeze
// window opens, so a crash anywhere between "workload stopped" and "stage
// TRANSFER persisted" still tells restart recovery whether this migration
// owns the freeze and since when the workload has been unreachable.
type migrationFreeze struct {
	MigrationID string `json:"migration_id"`
	WorkloadID  string `json:"workload_id"`
	// OwnsFreeze reports that the workload was running when this migration
	// admitted it, so this migration stopped it and is the only one allowed
	// to resume it. A workload the operator had paused is never owned.
	OwnsFreeze bool `json:"owns_freeze"`
	// DowntimeStart is the earliest instant this migration made the workload
	// unreachable: the drain that stopped its forwarders, the freeze, or the
	// checkpoint start that bounds an already-paused workload. Zero until the
	// migration actually caused downtime.
	DowntimeStart time.Time `json:"downtime_start,omitzero"`
	RecordedAt    time.Time `json:"recorded_at"`
}

// noteFreezeOwnership persists whether this migration owns its workload's
// freeze. It must run before the freeze window opens — a crash between the
// freeze and the first post-freeze stage persist is exactly the window this
// record exists to cover.
func (o *Orchestrator) noteFreezeOwnership(id, workloadID string, owns bool) {
	fact := migrationFreeze{
		MigrationID: id, WorkloadID: workloadID, OwnsFreeze: owns,
		RecordedAt: time.Now().UTC(),
	}
	if existing, err := o.freezes.Get(id); err == nil {
		fact.DowntimeStart = existing.DowntimeStart
		if !existing.RecordedAt.IsZero() {
			fact.RecordedAt = existing.RecordedAt
		}
	}
	if err := o.freezes.Put(id, fact); err != nil {
		o.logger.Warn("could not persist migration freeze ownership",
			"migration_id", id, "error", err)
	}
}

// noteDowntimeStart records the current downtime bound, replacing the
// previous one: the run path refines the bound from "checkpoint start" to
// "actual freeze instant" as better facts arrive, and the last persisted
// bound is what recovery measures the frozen outage from.
func (o *Orchestrator) noteDowntimeStart(id string, at time.Time) {
	if at.IsZero() {
		return
	}
	fact := migrationFreeze{MigrationID: id, DowntimeStart: at, RecordedAt: at}
	if existing, err := o.freezes.Get(id); err == nil {
		fact = existing
		fact.DowntimeStart = at
	}
	if err := o.freezes.Put(id, fact); err != nil {
		o.logger.Warn("could not persist migration downtime bound",
			"migration_id", id, "error", err)
	}
}

// freezeState returns the persisted freeze facts for a migration.
func (o *Orchestrator) freezeState(id string) (migrationFreeze, bool) {
	fact, err := o.freezes.Get(id)
	if err != nil {
		return migrationFreeze{}, false
	}
	return fact, true
}
