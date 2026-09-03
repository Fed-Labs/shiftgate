package update

import "testing"

func TestParseVersionAcceptsSemanticVersions(t *testing.T) {
	cases := map[string]string{
		"1.2.3":                  "1.2.3",
		"v0.1.0":                 "0.1.0",
		" 2.0.0 ":                "2.0.0",
		"1.0.0-beta.2":           "1.0.0-beta.2",
		"1.0.0-rc1+build.7":      "1.0.0-rc1+build.7",
		"0.10.0":                 "0.10.0",
		"10.20.30-alpha.1.build": "10.20.30-alpha.1.build",
	}
	for input, expected := range cases {
		version, err := ParseVersion(input)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", input, err)
		}
		if version.String() != expected {
			t.Fatalf("ParseVersion(%q) = %s, want %s", input, version, expected)
		}
	}
}

func TestParseVersionRejectsMalformedInput(t *testing.T) {
	for _, input := range []string{
		"", "v", "1", "1.2", "1.2.3.4", "01.2.3", "1.02.3", "1.2.x",
		"1.2.3-", "1.2.3+", "1.2.3-beta..1", "1.2.3-beta_1", "-1.2.3",
	} {
		if version, err := ParseVersion(input); err == nil {
			t.Fatalf("ParseVersion(%q) unexpectedly succeeded as %s", input, version)
		}
	}
}

func TestVersionPrecedenceFollowsSemanticVersioning(t *testing.T) {
	ordered := []string{
		"0.9.0",
		"0.10.0",
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11",
		"1.0.0-rc.1",
		"1.0.0",
		"1.0.1",
		"1.1.0",
		"2.0.0",
	}
	for index := 1; index < len(ordered); index++ {
		older, err := ParseVersion(ordered[index-1])
		if err != nil {
			t.Fatal(err)
		}
		newer, err := ParseVersion(ordered[index])
		if err != nil {
			t.Fatal(err)
		}
		if !older.Precedes(newer) {
			t.Fatalf("%s should precede %s", older, newer)
		}
		if newer.Precedes(older) {
			t.Fatalf("%s must not precede %s", newer, older)
		}
	}
}

func TestBuildMetadataDoesNotAffectPrecedence(t *testing.T) {
	plain, err := ParseVersion("1.4.2")
	if err != nil {
		t.Fatal(err)
	}
	built, err := ParseVersion("1.4.2+20260304.gitabcdef")
	if err != nil {
		t.Fatal(err)
	}
	if plain.Compare(built) != 0 {
		t.Fatalf("build metadata changed precedence: %s vs %s", plain, built)
	}
	if !plain.SameRelease(built) {
		t.Fatal("two builds of one version must be the same release")
	}
	if plain.Precedes(built) || built.Precedes(plain) {
		t.Fatal("neither build may be treated as an upgrade over the other")
	}
}

func TestPreReleaseIsNotAnUpgradeOverItsRelease(t *testing.T) {
	release, err := ParseVersion("1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	preRelease, err := ParseVersion("1.0.0-nightly.20260304")
	if err != nil {
		t.Fatal(err)
	}
	if release.Precedes(preRelease) {
		t.Fatal("a nightly build must not be treated as newer than the stable release")
	}
	if preRelease.Stable() {
		t.Fatal("a pre-release must not report itself as stable")
	}
}
