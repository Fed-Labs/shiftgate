package update

import (
	"fmt"
	"testing"
	"time"
)

func stagedRollout(percents ...int) Rollout {
	rollout := Rollout{}
	for index, percent := range percents {
		rollout.Stages = append(rollout.Stages, RolloutStage{
			StartsAt: testNow.Add(time.Duration(index-len(percents)) * time.Hour),
			Percent:  percent,
		})
	}
	return rollout
}

func TestCohortIsDeterministicAndVersionSpecific(t *testing.T) {
	version, err := ParseVersion("1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	first := Cohort("machine-a", version)
	if first != Cohort("machine-a", version) {
		t.Fatal("the same machine and version must land in the same bucket every time")
	}
	if first < 0 || first >= cohortBuckets {
		t.Fatalf("cohort %d is outside 0..%d", first, cohortBuckets-1)
	}
	other, err := ParseVersion("1.2.4")
	if err != nil {
		t.Fatal(err)
	}
	differing := 0
	for index := 0; index < 200; index++ {
		machine := fmt.Sprintf("machine-%d", index)
		if Cohort(machine, version) != Cohort(machine, other) {
			differing++
		}
	}
	// A machine that was last in one rollout must not be last in every rollout.
	if differing < 190 {
		t.Fatalf("only %d of 200 machines changed bucket between releases", differing)
	}
}

func TestCohortsSpreadAcrossTheFleet(t *testing.T) {
	version, err := ParseVersion("2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	rollout := stagedRollout(10)
	eligible := 0
	const machines = 2000
	for index := 0; index < machines; index++ {
		if rollout.Status(fmt.Sprintf("machine-%d", index), version, testNow).Eligible {
			eligible++
		}
	}
	// A 10% stage should reach roughly a tenth of the fleet. The bounds are wide
	// enough that a fair hash never fails the test and a broken one always does.
	if eligible < machines/20 || eligible > machines/5 {
		t.Fatalf("a 10%% stage reached %d of %d machines", eligible, machines)
	}
}

func TestRolloutWithoutStagesIsFullyOpen(t *testing.T) {
	version, err := ParseVersion("1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	status := Rollout{}.Status("any-machine", version, testNow)
	if !status.Eligible || status.PercentOpen != 100 {
		t.Fatalf("an unstaged release must be open to everyone, got %+v", status)
	}
	if status.NextStageAt != nil {
		t.Fatal("an unstaged release has no next stage")
	}
}

func TestRolloutReportsWhenItNextWidens(t *testing.T) {
	version, err := ParseVersion("1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	future := testNow.Add(2 * time.Hour)
	rollout := Rollout{Stages: []RolloutStage{
		{StartsAt: testNow.Add(-time.Hour), Percent: 0},
		{StartsAt: future, Percent: 100},
	}}
	status := rollout.Status("machine-a", version, testNow)
	if status.Eligible {
		t.Fatal("a machine must not be eligible while the rollout is at 0%")
	}
	if status.NextStageAt == nil || !status.NextStageAt.Equal(future) {
		t.Fatalf("expected next stage at %s, got %+v", future, status.NextStageAt)
	}
}

func TestRolloutValidationRefusesShrinkingExposure(t *testing.T) {
	rollout := Rollout{Stages: []RolloutStage{
		{StartsAt: testNow.Add(-2 * time.Hour), Percent: 50},
		{StartsAt: testNow.Add(-time.Hour), Percent: 10},
	}}
	if err := rollout.Validate(); err == nil {
		t.Fatal("a rollout that reduces exposure must be refused")
	}
}

func TestRolloutValidationRefusesMalformedPlans(t *testing.T) {
	duplicate := Rollout{Stages: []RolloutStage{
		{StartsAt: testNow, Percent: 10},
		{StartsAt: testNow, Percent: 20},
	}}
	if err := duplicate.Validate(); err == nil {
		t.Fatal("two stages at the same instant must be refused")
	}
	missingTime := Rollout{Stages: []RolloutStage{{Percent: 10}}}
	if err := missingTime.Validate(); err == nil {
		t.Fatal("a stage without a start time must be refused")
	}
	outOfRange := Rollout{Stages: []RolloutStage{{StartsAt: testNow, Percent: 101}}}
	if err := outOfRange.Validate(); err == nil {
		t.Fatal("a percentage above 100 must be refused")
	}
	tooMany := Rollout{}
	for index := 0; index <= maxRolloutStages; index++ {
		tooMany.Stages = append(tooMany.Stages, RolloutStage{
			StartsAt: testNow.Add(time.Duration(index) * time.Minute),
			Percent:  1,
		})
	}
	if err := tooMany.Validate(); err == nil {
		t.Fatalf("more than %d stages must be refused", maxRolloutStages)
	}
}

func TestRolloutNormalizationOrdersStages(t *testing.T) {
	rollout := Rollout{Stages: []RolloutStage{
		{StartsAt: testNow.Add(time.Hour), Percent: 100},
		{StartsAt: testNow.Add(-time.Hour), Percent: 25},
	}}
	if err := rollout.Validate(); err != nil {
		t.Fatalf("a plan given out of order should normalize, got %v", err)
	}
	if rollout.Stages[0].Percent != 25 {
		t.Fatalf("stages were not ordered chronologically: %+v", rollout.Stages)
	}
	if percent := rollout.PercentAt(testNow); percent != 25 {
		t.Fatalf("expected 25%% open at the test instant, got %d", percent)
	}
	if percent := rollout.PercentAt(testNow.Add(2 * time.Hour)); percent != 100 {
		t.Fatalf("expected the plan to be fully open later, got %d", percent)
	}
}
