package update

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"time"
)

// cohortBuckets is how finely machines are spread across a rollout. Percentages
// are integers, so ten thousand buckets leaves two decimal places of headroom
// between a percentage and the bucket boundary it maps to.
const cohortBuckets = 10000

// maxRolloutStages bounds a rollout plan. A feed that asks for more is
// malformed rather than merely ambitious.
const maxRolloutStages = 32

// RolloutStage opens a release to a percentage of the fleet from a point in
// time. Machines are assigned to a bucket deterministically, so the same machine
// is offered the same release at the same stage no matter how often it checks.
type RolloutStage struct {
	StartsAt time.Time `json:"starts_at"`
	Percent  int       `json:"percent"`
}

// Rollout is the staged plan for one release. An empty plan means the release is
// available to every machine immediately, which is what a hotfix wants.
type Rollout struct {
	Stages []RolloutStage `json:"stages,omitempty"`
}

// RolloutStatus explains a machine's position in a rollout. It exists so an
// operator can see why an available release has not been offered yet, instead of
// having to guess whether the update system is working.
type RolloutStatus struct {
	Cohort      int        `json:"cohort"`
	Buckets     int        `json:"buckets"`
	PercentOpen int        `json:"percent_open"`
	Eligible    bool       `json:"eligible"`
	NextStageAt *time.Time `json:"next_stage_at,omitempty"`
}

// Normalize puts the stages in chronological order and drops sub-second
// precision, so a plan has one canonical form to sign.
func (rollout *Rollout) Normalize() {
	for index := range rollout.Stages {
		rollout.Stages[index].StartsAt = rollout.Stages[index].StartsAt.UTC().Truncate(time.Second)
	}
	sort.SliceStable(rollout.Stages, func(first, second int) bool {
		return rollout.Stages[first].StartsAt.Before(rollout.Stages[second].StartsAt)
	})
}

// Validate checks a rollout plan. A plan whose exposure shrinks is refused: a
// machine that was already offered a release must not have it taken away, since
// it may already be running it.
func (rollout *Rollout) Validate() error {
	rollout.Normalize()
	if len(rollout.Stages) > maxRolloutStages {
		return fmt.Errorf("a rollout may define at most %d stages, got %d", maxRolloutStages, len(rollout.Stages))
	}
	previousPercent := 0
	var previousStart time.Time
	for index, stage := range rollout.Stages {
		if stage.StartsAt.IsZero() {
			return fmt.Errorf("stage %d requires a start time", index)
		}
		if stage.Percent < 0 || stage.Percent > 100 {
			return fmt.Errorf("stage %d percent %d is outside 0..100", index, stage.Percent)
		}
		if index > 0 {
			if !stage.StartsAt.After(previousStart) {
				return fmt.Errorf("stage %d does not start after the previous stage", index)
			}
			if stage.Percent < previousPercent {
				return errors.New("a rollout may not reduce the percentage it already opened")
			}
		}
		previousPercent = stage.Percent
		previousStart = stage.StartsAt
	}
	return nil
}

// PercentAt reports how much of the fleet the release is open to at a point in
// time. A plan with no stages is fully open.
func (rollout Rollout) PercentAt(now time.Time) int {
	if len(rollout.Stages) == 0 {
		return 100
	}
	percent := 0
	for _, stage := range rollout.Stages {
		if stage.StartsAt.After(now) {
			break
		}
		percent = stage.Percent
	}
	return percent
}

// NextStageAfter reports when the rollout next widens, so a machine that is not
// yet eligible can say when it expects to be.
func (rollout Rollout) NextStageAfter(now time.Time) *time.Time {
	for _, stage := range rollout.Stages {
		if stage.StartsAt.After(now) {
			startsAt := stage.StartsAt
			return &startsAt
		}
	}
	return nil
}

// Cohort assigns a machine to a bucket for one release. The assignment is a
// digest of the machine identity and the release version together, so a machine
// that landed late in one rollout is not systematically last in every rollout.
func Cohort(machineID string, version Version) int {
	digest := sha256.Sum256([]byte(machineID + "\x00" + version.String()))
	return int(binary.BigEndian.Uint64(digest[:8]) % cohortBuckets)
}

// Status places one machine in one rollout at one point in time.
func (rollout Rollout) Status(machineID string, version Version, now time.Time) RolloutStatus {
	cohort := Cohort(machineID, version)
	percent := rollout.PercentAt(now)
	return RolloutStatus{
		Cohort:      cohort,
		Buckets:     cohortBuckets,
		PercentOpen: percent,
		Eligible:    cohort < percent*(cohortBuckets/100),
		NextStageAt: rollout.NextStageAfter(now),
	}
}
