package update

import (
	"testing"
	"time"
)

// evaluateOne evaluates a single release for an installation and returns the verdict.
func evaluateOne(t *testing.T, feed Feed, installation Installation, ring *KeyRing) Evaluation {
	t.Helper()
	evaluations, err := Evaluate(feed, installation, ring, testNow)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(evaluations) != 1 {
		t.Fatalf("expected one evaluation, got %d", len(evaluations))
	}
	return evaluations[0]
}

func requireSkip(t *testing.T, evaluation Evaluation, code string) {
	t.Helper()
	if evaluation.Applicable {
		t.Fatalf("release %s should not be applicable", evaluation.Version)
	}
	if evaluation.Code != code {
		t.Fatalf("expected %s, got %s (%s)", code, evaluation.Code, evaluation.Reason)
	}
	if evaluation.Artifact != nil {
		t.Fatal("an inapplicable release must not carry an artifact to install")
	}
}

func agentRelease(version string) Release {
	return releaseFor(version, binaryArtifact(ComponentAgent, "https://releases.example/agent-"+version, versionScript("shift-agent", version)))
}

func TestApplicableReleaseCarriesItsArtifact(t *testing.T) {
	signer := newSigner(t)
	feed := signedFeed(t, signer, agentRelease("1.1.0"))
	evaluation := evaluateOne(t, feed, installationFor(ComponentAgent, "1.0.0", "/usr/bin/shift-agent"), keyRingFor(t, 1, signer))
	if !evaluation.Applicable {
		t.Fatalf("expected an applicable release, got %s (%s)", evaluation.Code, evaluation.Reason)
	}
	if evaluation.Artifact == nil || evaluation.Artifact.Component != ComponentAgent {
		t.Fatalf("expected an agent artifact, got %+v", evaluation.Artifact)
	}
	if len(evaluation.SignedBy) != 1 {
		t.Fatalf("expected the verifying key to be reported, got %v", evaluation.SignedBy)
	}
	if !evaluation.Rollout.Eligible {
		t.Fatal("an unstaged release must be open to this machine")
	}
}

func TestUntrustedReleaseIsSkippedBeforeAnyOtherDecision(t *testing.T) {
	trusted := newSigner(t)
	attacker := newSigner(t)
	feed := signedFeed(t, attacker, agentRelease("2.0.0"))
	evaluation := evaluateOne(t, feed, installationFor(ComponentAgent, "1.0.0", "/usr/bin/shift-agent"), keyRingFor(t, 1, trusted))
	requireSkip(t, evaluation, SkipUntrusted)
}

func TestChannelMismatchIsSkipped(t *testing.T) {
	signer := newSigner(t)
	release := agentRelease("1.1.0")
	release.Channel = ChannelBeta
	signed, err := signer.SignRelease(release)
	if err != nil {
		t.Fatal(err)
	}
	feed := Feed{Version: feedSchemaVersion, Channel: ChannelBeta, GeneratedAt: testNow, Releases: []SignedRelease{signed}}
	evaluation := evaluateOne(t, feed, installationFor(ComponentAgent, "1.0.0", "/usr/bin/shift-agent"), keyRingFor(t, 1, signer))
	requireSkip(t, evaluation, SkipChannel)
}

func TestRevokedReleaseIsSkipped(t *testing.T) {
	signer := newSigner(t)
	feed := signedFeed(t, signer, agentRelease("1.1.0"))
	feed.Revoked = []string{"1.1.0"}
	evaluation := evaluateOne(t, feed, installationFor(ComponentAgent, "1.0.0", "/usr/bin/shift-agent"), keyRingFor(t, 1, signer))
	requireSkip(t, evaluation, SkipRevoked)
}

func TestInstalledVersionIsNotReinstalled(t *testing.T) {
	signer := newSigner(t)
	feed := signedFeed(t, signer, agentRelease("1.1.0"))
	ring := keyRingFor(t, 1, signer)
	same := evaluateOne(t, feed, installationFor(ComponentAgent, "1.1.0", "/usr/bin/shift-agent"), ring)
	requireSkip(t, same, SkipNotNewer)
	newer := evaluateOne(t, feed, installationFor(ComponentAgent, "1.2.0", "/usr/bin/shift-agent"), ring)
	requireSkip(t, newer, SkipNotNewer)

	// A rollback deliberately installs an older version, so it opts out.
	downgrade := installationFor(ComponentAgent, "1.2.0", "/usr/bin/shift-agent")
	downgrade.AllowDowngrade = true
	if evaluation := evaluateOne(t, feed, downgrade, ring); !evaluation.Applicable {
		t.Fatalf("a rollback must be able to install an older release, got %s (%s)", evaluation.Code, evaluation.Reason)
	}
}

func TestPreReleaseIsSkippedOnStable(t *testing.T) {
	signer := newSigner(t)
	feed := signedFeed(t, signer, agentRelease("1.2.0-rc.1"))
	evaluation := evaluateOne(t, feed, installationFor(ComponentAgent, "1.1.0", "/usr/bin/shift-agent"), keyRingFor(t, 1, signer))
	requireSkip(t, evaluation, SkipPreRelease)
}

func TestMissingComponentAndPlatformAreDistinguished(t *testing.T) {
	signer := newSigner(t)
	ring := keyRingFor(t, 1, signer)
	desktopOnly := releaseFor("1.1.0", binaryArtifact(ComponentDesktop, "https://releases.example/desktop", versionScript("shift-desktop", "1.1.0")))
	missingComponent := evaluateOne(t, signedFeed(t, signer, desktopOnly), installationFor(ComponentAgent, "1.0.0", "/usr/bin/shift-agent"), ring)
	requireSkip(t, missingComponent, SkipComponent)

	otherPlatform := agentRelease("1.1.0")
	otherPlatform.Artifacts[0].Architecture = "arm64"
	wrongPlatform := evaluateOne(t, signedFeed(t, signer, otherPlatform), installationFor(ComponentAgent, "1.0.0", "/usr/bin/shift-agent"), ring)
	requireSkip(t, wrongPlatform, SkipPlatform)
}

func TestRequiredIntermediateVersionIsEnforced(t *testing.T) {
	signer := newSigner(t)
	ring := keyRingFor(t, 1, signer)
	release := agentRelease("3.0.0")
	release.MinimumFromVersion = "2.5.0"
	feed := signedFeed(t, signer, release)
	tooOld := evaluateOne(t, feed, installationFor(ComponentAgent, "2.0.0", "/usr/bin/shift-agent"), ring)
	requireSkip(t, tooOld, SkipUpgradePath)
	if evaluation := evaluateOne(t, feed, installationFor(ComponentAgent, "2.5.0", "/usr/bin/shift-agent"), ring); !evaluation.Applicable {
		t.Fatalf("the required intermediate version must be enough, got %s (%s)", evaluation.Code, evaluation.Reason)
	}
}

func TestReleaseThatCannotSpeakToTheControlPlaneIsSkipped(t *testing.T) {
	signer := newSigner(t)
	ring := keyRingFor(t, 1, signer)
	release := agentRelease("2.0.0")
	release.MinimumProtocolVersion = 3
	release.ProtocolVersion = 4
	installation := installationFor(ComponentAgent, "1.0.0", "/usr/bin/shift-agent")
	installation.PeerProtocolVersion = 2
	requireSkip(t, evaluateOne(t, signedFeed(t, signer, release), installation, ring), SkipProtocol)

	installation.PeerProtocolVersion = 3
	if evaluation := evaluateOne(t, signedFeed(t, signer, release), installation, ring); !evaluation.Applicable {
		t.Fatalf("a release inside the protocol range must be applicable, got %s (%s)", evaluation.Code, evaluation.Reason)
	}
}

func TestReleaseThatCannotReadLocalSchemasIsSkipped(t *testing.T) {
	signer := newSigner(t)
	ring := keyRingFor(t, 1, signer)

	olderConfig := agentRelease("2.0.0")
	olderConfig.ConfigVersion = 1
	installation := installationFor(ComponentAgent, "1.0.0", "/usr/bin/shift-agent")
	installation.ConfigVersion = 2
	requireSkip(t, evaluateOne(t, signedFeed(t, signer, olderConfig), installation, ring), SkipConfigSchema)

	olderState := agentRelease("2.0.0")
	olderState.StateVersion = 1
	stateInstallation := installationFor(ComponentAgent, "1.0.0", "/usr/bin/shift-agent")
	stateInstallation.StateVersion = 3
	requireSkip(t, evaluateOne(t, signedFeed(t, signer, olderState), stateInstallation, ring), SkipStateSchema)
}

func TestMachineOutsideTheRolloutCohortWaits(t *testing.T) {
	signer := newSigner(t)
	release := agentRelease("1.5.0")
	release.Rollout = Rollout{Stages: []RolloutStage{
		{StartsAt: testNow.Add(-time.Hour), Percent: 0},
		{StartsAt: testNow.Add(time.Hour), Percent: 100},
	}}
	evaluation := evaluateOne(t, signedFeed(t, signer, release), installationFor(ComponentAgent, "1.0.0", "/usr/bin/shift-agent"), keyRingFor(t, 1, signer))
	requireSkip(t, evaluation, SkipRolloutCohort)
	if evaluation.Rollout.NextStageAt == nil {
		t.Fatal("a waiting machine should be told when the rollout widens")
	}
}

func TestSelectTakesTheNewestApplicableRelease(t *testing.T) {
	signer := newSigner(t)
	blocked := agentRelease("3.0.0")
	blocked.MinimumFromVersion = "2.0.0"
	feed := signedFeed(t, signer, agentRelease("1.1.0"), blocked, agentRelease("1.4.0"))
	evaluations, err := Evaluate(feed, installationFor(ComponentAgent, "1.0.0", "/usr/bin/shift-agent"), keyRingFor(t, 1, signer), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if evaluations[0].Version != "3.0.0" {
		t.Fatalf("evaluations must be newest first, got %s", evaluations[0].Version)
	}
	selected, ok := Select(evaluations)
	if !ok {
		t.Fatal("expected a selectable release")
	}
	if selected.Version != "1.4.0" {
		t.Fatalf("expected the newest applicable release 1.4.0, got %s", selected.Version)
	}
}

func TestSelectReportsNothingWhenTheMachineIsCurrent(t *testing.T) {
	signer := newSigner(t)
	feed := signedFeed(t, signer, agentRelease("1.1.0"))
	evaluations, err := Evaluate(feed, installationFor(ComponentAgent, "1.1.0", "/usr/bin/shift-agent"), keyRingFor(t, 1, signer), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Select(evaluations); ok {
		t.Fatal("a current machine must not select an update")
	}
}

func TestFeedValidationRefusesUnsignedAndUnknownDocuments(t *testing.T) {
	signer := newSigner(t)
	feed := signedFeed(t, signer, agentRelease("1.1.0"))

	unknownVersion := feed
	unknownVersion.Version = 2
	if err := unknownVersion.Validate(); err == nil {
		t.Fatal("an unknown feed schema must be refused")
	}

	unsigned := signedFeed(t, signer, agentRelease("1.1.0"))
	unsigned.Releases[0].Signatures = nil
	if err := unsigned.Validate(); err == nil {
		t.Fatal("a release with no signatures must be refused by the feed")
	}

	mismatched := signedFeed(t, signer, agentRelease("1.1.0"))
	mismatched.Channel = ChannelNightly
	if err := mismatched.Validate(); err == nil {
		t.Fatal("a feed must not carry releases from another channel")
	}

	if err := feed.Validate(); err != nil {
		t.Fatalf("a well-formed feed must validate: %v", err)
	}
}

func TestEvaluateRequiresAKeyRing(t *testing.T) {
	signer := newSigner(t)
	feed := signedFeed(t, signer, agentRelease("1.1.0"))
	if _, err := Evaluate(feed, installationFor(ComponentAgent, "1.0.0", "/usr/bin/shift-agent"), nil, testNow); err == nil {
		t.Fatal("evaluating without a key ring must be an error, not an unverified pass")
	}
}
