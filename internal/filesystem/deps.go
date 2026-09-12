package filesystem

import (
	"bufio"
	"debug/elf"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"shift.dev/shift/internal/model"
)

// librarySearchDirs are the subdirectories of a workload root that are
// searched for a self-contained copy of a binary or library, in the order a
// modern glibc system would use.
var librarySearchDirs = []string{
	"lib/x86_64-linux-gnu", "usr/lib/x86_64-linux-gnu",
	"lib64", "usr/lib64", "lib", "usr/lib",
}

// systemLibraryDirs are the host's default library directories, as the
// dynamic loader searches them after LD_LIBRARY_PATH.
var systemLibraryDirs = []string{
	"/lib/x86_64-linux-gnu", "/usr/lib/x86_64-linux-gnu",
	"/lib64", "/usr/lib64", "/lib", "/usr/lib",
}

// Discovery is the outcome of dependency analysis: the files the workload's
// command needs that could be located, and the ones that could not. Both are
// reported — a library SHIFT cannot find is information the operator needs,
// not something to smooth over.
type Discovery struct {
	Dependencies []model.FileDependency
	Unresolved   []string
}

// DependencyKind values recorded on each dependency.
const (
	DependencyBinary      = "binary"
	DependencyInterpreter = "interpreter"
	DependencyLibrary     = "library"
	DependencyScript      = "script"
)

// DiscoverDependencies walks a workload command's executable dependency tree:
// the binary itself, its ELF interpreter, every DT_NEEDED shared library
// (recursively), or — for a script — its shebang interpreter. Files inside
// the workload root are marked as such: they travel with the checkpoint.
// Files outside it do not, and a destination without them cannot run the
// workload, which is exactly what the manifest records them for.
func DiscoverDependencies(command []string, root string, environment []string) (Discovery, error) {
	discovery := Discovery{Dependencies: make([]model.FileDependency, 0, 8)}
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return discovery, fmt.Errorf("dependency discovery requires a command")
	}
	root = filepath.Clean(root)
	searched := make(map[string]struct{})
	var walk func(path, kind string) error
	walk = func(path, kind string) error {
		if _, seen := searched[path]; seen {
			return nil
		}
		searched[path] = struct{}{}
		info, err := os.Stat(path)
		if err != nil {
			discovery.Unresolved = append(discovery.Unresolved, path)
			return nil
		}
		discovery.Dependencies = append(discovery.Dependencies, model.FileDependency{
			Path: path, Kind: kind, InsideRoot: pathWithinRoot(root, path), SizeBytes: info.Size(),
		})
		needed, interpreter, err := elfDependencies(path)
		if err != nil {
			// Not an ELF file. A script's shebang still points at a real
			// dependency; anything else simply has none.
			if script, scriptErr := shebangInterpreter(path); scriptErr == nil && script != "" {
				resolved, resolveErr := resolveCommand(script, root)
				if resolveErr != nil {
					discovery.Unresolved = append(discovery.Unresolved, script)
					return nil
				}
				return walk(resolved, DependencyInterpreter)
			}
			return nil
		}
		if interpreter != "" {
			resolved := interpreter
			if !filepath.IsAbs(interpreter) {
				resolved = filepath.Clean("/" + interpreter)
			}
			if err := walk(resolved, DependencyInterpreter); err != nil {
				return err
			}
		}
		for _, library := range needed {
			resolved, found := resolveLibrary(library, root, environment)
			if !found {
				discovery.Unresolved = append(discovery.Unresolved, library)
				continue
			}
			if err := walk(resolved, DependencyLibrary); err != nil {
				return err
			}
		}
		return nil
	}
	binary, err := resolveCommand(command[0], root)
	if err != nil {
		discovery.Unresolved = append(discovery.Unresolved, command[0])
		return discovery, nil
	}
	if err := walk(binary, DependencyBinary); err != nil {
		return discovery, err
	}
	return discovery, nil
}

// elfDependencies returns the DT_NEEDED libraries and PT_INTERP interpreter of
// an ELF file. A non-ELF file is reported as such, not as an error.
func elfDependencies(path string) (needed []string, interpreter string, err error) {
	file, err := elf.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = file.Close() }()
	for _, program := range file.Progs {
		if program.Type == elf.PT_INTERP {
			raw := make([]byte, program.Filesz)
			if _, err := program.ReadAt(raw, 0); err != nil {
				return nil, "", err
			}
			interpreter = strings.TrimRight(string(raw), "\x00")
		}
	}
	dynamic := file.SectionByType(elf.SHT_DYNAMIC)
	if dynamic == nil {
		return nil, interpreter, nil
	}
	libraries, err := file.DynString(elf.DT_NEEDED)
	if err != nil {
		return nil, interpreter, err
	}
	return libraries, interpreter, nil
}

// shebangInterpreter reads a script's #! line and returns the interpreter
// path it names. An empty result means the file is not a shebang script.
func shebangInterpreter(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReader(io.LimitReader(file, 256))
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "#!") {
		return "", nil
	}
	// The interpreter is the first token; its optional single argument
	// (e.g. `#!/usr/bin/env python3`) names the real binary.
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(fields) == 0 {
		return "", nil
	}
	if filepath.Base(fields[0]) == "env" && len(fields) > 1 {
		return fields[1], nil
	}
	return fields[0], nil
}

// resolveCommand locates the executable a workload names: an absolute path
// resolves directly, a path inside the workload root is honored, and a bare
// name is resolved through PATH like the runtime manager does when starting
// the process.
func resolveCommand(command, root string) (string, error) {
	if filepath.IsAbs(command) {
		return command, nil
	}
	if strings.ContainsRune(command, os.PathSeparator) {
		return filepath.Abs(command)
	}
	if resolved, err := exec.LookPath(command); err == nil {
		return resolved, nil
	}
	// Last resort: an executable inside the workload root. A workload may
	// carry its own tools directory and run them by bare name via PATH —
	// discovering that copy is exactly what root-relative lookup is for.
	for _, directory := range librarySearchDirs {
		candidate := filepath.Join(root, directory, command)
		if info, err := os.Stat(candidate); err == nil && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("resolve command %q", command)
}

// resolveLibrary finds a DT_NEEDED library, preferring copies inside the
// workload root — a self-contained root's libraries travel with the
// checkpoint, the host's do not.
func resolveLibrary(name, root string, environment []string) (string, bool) {
	directories := make([]string, 0, len(librarySearchDirs)*2+8)
	for _, directory := range librarySearchDirs {
		directories = append(directories, filepath.Join(root, directory))
	}
	directories = append(directories, libraryPaths(environment)...)
	for _, directory := range directories {
		candidate := filepath.Join(directory, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

// libraryPaths extracts LD_LIBRARY_PATH directories from the environment the
// workload runs with, so a workload that carries its own library directory is
// searched the same way the loader searches it. The loader's default system
// directories are appended after them.
func libraryPaths(environment []string) []string {
	var paths []string
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if !found || key != "LD_LIBRARY_PATH" {
			continue
		}
		for _, directory := range filepath.SplitList(value) {
			if directory == "" {
				continue
			}
			if !filepath.IsAbs(directory) {
				directory = "/" + directory
			}
			paths = append(paths, filepath.Clean(directory))
		}
	}
	// The loader searches LD_LIBRARY_PATH and then the default directories;
	// discovery searches the same way so it finds what the process finds.
	paths = append(paths, systemLibraryDirs...)
	return paths
}

func pathWithinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
