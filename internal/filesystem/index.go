package filesystem

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// FileStamp is the cheap identity of one file: the attributes that change
// when its content changes. Size plus modification time plus mode plus inode
// is the same basis rsync uses for its quick check; it can theoretically miss
// a same-size same-mtime rewrite, so it is used to describe the delta to
// operators — never as a correctness-critical dedup decision.
type FileStamp struct {
	Size     int64  `json:"size"`
	MtimeSec int64  `json:"mtime_sec"`
	MtimeNS  int64  `json:"mtime_nsec"`
	Mode     uint32 `json:"mode"`
	Inode    uint64 `json:"inode"`
	Device   uint64 `json:"device"`
}

// TreeIndex maps every file under a root (relative to the root) to its stamp.
type TreeIndex map[string]FileStamp

// IndexTree walks a workload root and stamps every file, honoring the same
// exclusion patterns the archive applies, so the index and the tarball always
// describe the same tree.
func IndexTree(root string, exclusions []string) (TreeIndex, error) {
	root = filepath.Clean(root)
	if root == string(filepath.Separator) {
		return nil, fmt.Errorf("refusing to index the filesystem root")
	}
	matcher := newExclusionMatcher(exclusions)
	index := make(TreeIndex, 64)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if matcher.excluded(relative) {
			// A pruned directory keeps everything beneath it out of the index,
			// exactly as tar --exclude keeps it out of the archive.
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		stamp := FileStamp{Mode: uint32(info.Mode())}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			stamp.Size = stat.Size
			stamp.MtimeSec, stamp.MtimeNS = stat.Mtim.Sec, stat.Mtim.Nsec
			stamp.Inode = stat.Ino
			stamp.Device = uint64(stat.Dev)
		} else {
			stamp.Size = info.Size()
		}
		index[filepath.ToSlash(relative)] = stamp
		return nil
	})
	if err != nil {
		return nil, err
	}
	return index, nil
}

// exclusionMatcher applies tar-style --exclude semantics: a pattern without a
// separator matches any single path component; a pattern with separators
// matches any trailing portion of the relative path.
type exclusionMatcher struct {
	segmentPatterns []string
	pathPatterns    []string
}

func newExclusionMatcher(exclusions []string) *exclusionMatcher {
	matcher := &exclusionMatcher{}
	for _, pattern := range exclusions {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if strings.ContainsRune(pattern, '/') {
			matcher.pathPatterns = append(matcher.pathPatterns, pattern)
		} else {
			matcher.segmentPatterns = append(matcher.segmentPatterns, pattern)
		}
	}
	return matcher
}

func (matcher *exclusionMatcher) excluded(relative string) bool {
	relative = filepath.ToSlash(relative)
	for _, pattern := range matcher.segmentPatterns {
		for _, segment := range strings.Split(relative, "/") {
			if match, err := filepath.Match(pattern, segment); err == nil && match {
				return true
			}
		}
	}
	for _, pattern := range matcher.pathPatterns {
		candidate := relative
		for {
			if match, err := filepath.Match(pattern, candidate); err == nil && match {
				return true
			}
			slash := strings.IndexByte(candidate, '/')
			if slash < 0 {
				break
			}
			candidate = candidate[slash+1:]
		}
	}
	return false
}

// TreeDiff is the file-level difference between two indexes of the same root.
type TreeDiff struct {
	Changed []string
	Added   []string
	Deleted []string
}

// IsZero reports that nothing changed at all.
func (diff *TreeDiff) IsZero() bool {
	return len(diff.Changed) == 0 && len(diff.Added) == 0 && len(diff.Deleted) == 0
}

// Diff compares the previous index of a root with its current one. A path
// whose stamp differs is changed, a path only the current index has is added,
// and a path only the previous index has is deleted. All three lists are
// sorted, so a diff is deterministic and comparable across runs.
func Diff(previous, current TreeIndex) TreeDiff {
	var diff TreeDiff
	for path, stamp := range current {
		before, existed := previous[path]
		switch {
		case !existed:
			diff.Added = append(diff.Added, path)
		case before != stamp:
			diff.Changed = append(diff.Changed, path)
		}
	}
	for path := range previous {
		if _, still := current[path]; !still {
			diff.Deleted = append(diff.Deleted, path)
		}
	}
	sort.Strings(diff.Changed)
	sort.Strings(diff.Added)
	sort.Strings(diff.Deleted)
	return diff
}

// EncodeIndex serializes an index for persistence between checkpoints.
func EncodeIndex(index TreeIndex) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	if err := encoder.Encode(index); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// DecodeIndex reads back an encoded index.
func DecodeIndex(encoded []byte) (TreeIndex, error) {
	var index TreeIndex
	if err := json.Unmarshal(encoded, &index); err != nil {
		return nil, err
	}
	return index, nil
}

// LoadIndex reads the persisted index for a workload. A missing index file is
// not an error: the returned index is nil and the caller knows the workload
// has no previous index to diff against.
func LoadIndex(path string) (TreeIndex, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return DecodeIndex(encoded)
}

// SaveIndex atomically persists the index for a workload's next diff.
func SaveIndex(path string, index TreeIndex) error {
	encoded, err := EncodeIndex(index)
	if err != nil {
		return err
	}
	staged := path + ".tmp"
	if err := os.WriteFile(staged, encoded, 0o600); err != nil {
		return err
	}
	return os.Rename(staged, path)
}
