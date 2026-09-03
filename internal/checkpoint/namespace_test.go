package checkpoint

import (
	"strings"
	"testing"
)

func TestNamespaceCommandBuildsFlatArgv(t *testing.T) {
	arguments, err := namespaceCommand("/usr/bin/shift-agent",
		[]BindMount{{Source: "/srv/app-fork", Target: "/srv/app"}, {Source: "/srv/models", Target: "/srv/app/models", ReadOnly: true}},
		"/usr/sbin/criu", []string{"restore", "--images-dir", "/var/lib/shift/forks/1/images"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		MountNamespaceCommand,
		"--bind", "/srv/app-fork", "/srv/app", "rw",
		"--bind", "/srv/models", "/srv/app/models", "ro",
		"--", "/usr/sbin/criu", "restore", "--images-dir", "/var/lib/shift/forks/1/images",
	}
	if len(arguments) != len(want) {
		t.Fatalf("got %v, want %v", arguments, want)
	}
	for index := range want {
		if arguments[index] != want[index] {
			t.Fatalf("argument %d is %q, want %q", index, arguments[index], want[index])
		}
	}
	// Arguments are a flat argv, so shell metacharacters stay literal data.
	round, program, programArgs, err := parseMountNamespaceArgs(arguments[1:])
	if err != nil {
		t.Fatal(err)
	}
	if program != "/usr/sbin/criu" || len(programArgs) != 3 || len(round) != 2 {
		t.Fatalf("round trip lost information: %v %v %v", round, program, programArgs)
	}
	if !round[1].ReadOnly || round[0].ReadOnly {
		t.Fatalf("read-only flags were not preserved: %+v", round)
	}
}

func TestNamespaceCommandRequiresAHelperBinary(t *testing.T) {
	if _, err := namespaceCommand("", nil, "/usr/sbin/criu", nil); err == nil {
		t.Fatal("an unknown helper binary must be rejected")
	}
}

func TestMountNamespaceArgumentValidation(t *testing.T) {
	cases := []struct {
		name      string
		arguments []string
		wantError string
	}{
		{name: "no program", arguments: []string{"--bind", "/a", "/b", "rw"}, wantError: "program"},
		{name: "empty program", arguments: []string{"--"}, wantError: "program"},
		{name: "truncated bind", arguments: []string{"--bind", "/a", "/b"}, wantError: "--bind requires"},
		{name: "relative source", arguments: []string{"--bind", "a", "/b", "rw", "--", "/bin/true"}, wantError: "absolute"},
		{name: "relative target", arguments: []string{"--bind", "/a", "b", "rw", "--", "/bin/true"}, wantError: "absolute"},
		{name: "empty target", arguments: []string{"--bind", "/a", "", "rw", "--", "/bin/true"}, wantError: "source and a target"},
		{name: "root target", arguments: []string{"--bind", "/a", "/", "rw", "--", "/bin/true"}, wantError: "filesystem root"},
		{name: "unknown flag", arguments: []string{"--mount", "/a", "--", "/bin/true"}, wantError: "unexpected"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, _, err := parseMountNamespaceArgs(testCase.arguments)
			if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("got %v, want an error containing %q", err, testCase.wantError)
			}
		})
	}
}

func TestMountNamespaceParsesAProgramWithoutMounts(t *testing.T) {
	mounts, program, programArgs, err := parseMountNamespaceArgs([]string{"--", "/usr/sbin/criu", "check"})
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 0 || program != "/usr/sbin/criu" || len(programArgs) != 1 || programArgs[0] != "check" {
		t.Fatalf("unexpected parse: %v %q %v", mounts, program, programArgs)
	}
}
