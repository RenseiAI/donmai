package workarea

import (
	"fmt"
	"math"
	"os"
)

// PhysicalUsage returns allocated bytes for the complete root. Any walk,
// metadata, or platform-accounting failure is returned; callers must not report
// a partial total as authoritative disk usage.
func PhysicalUsage(root RootPath) (int64, error) {
	handle, err := OpenRootExact(root, FileIdentity{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = handle.Close() }()
	return PhysicalUsageRoot(handle)
}

// PhysicalUsageRoot returns physical allocation through one pinned descriptor.
func PhysicalUsageRoot(root *os.Root) (int64, error) {
	if root == nil {
		return 0, fmt.Errorf("runtime/workarea: physical usage root descriptor is required")
	}
	seen := make(map[FileIdentity]struct{})
	total, err := physicalUsageDirectory(root, seen)
	if err != nil {
		return 0, fmt.Errorf("runtime/workarea: physical usage: %w", err)
	}
	return total, nil
}

func physicalUsageDirectory(root *os.Root, seen map[FileIdentity]struct{}) (int64, error) {
	rootInfo, err := root.Stat(".")
	if err != nil {
		return 0, err
	}
	total, err := accountPhysicalInfo(rootInfo, seen)
	if err != nil {
		return 0, err
	}
	directory, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	entries, err := directory.ReadDir(-1)
	if closeErr := directory.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		info, err := root.Lstat(entry.Name())
		if err != nil {
			return 0, err
		}
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			child, err := root.OpenRoot(entry.Name())
			if err != nil {
				return 0, err
			}
			openedInfo, err := child.Stat(".")
			if err != nil || !os.SameFile(info, openedInfo) {
				_ = child.Close()
				return 0, fmt.Errorf("physical usage directory identity changed")
			}
			childTotal, childErr := physicalUsageDirectory(child, seen)
			_ = child.Close()
			if childErr != nil {
				return 0, childErr
			}
			if childTotal > 0 && total > math.MaxInt64-childTotal {
				return 0, fmt.Errorf("physical usage overflow")
			}
			total += childTotal
			continue
		}
		allocated, err := accountPhysicalInfo(info, seen)
		if err != nil {
			return 0, err
		}
		if allocated > 0 && total > math.MaxInt64-allocated {
			return 0, fmt.Errorf("physical usage overflow")
		}
		total += allocated
	}
	return total, nil
}

func accountPhysicalInfo(info os.FileInfo, seen map[FileIdentity]struct{}) (int64, error) {
	identity, err := fileIdentity(info)
	if err != nil {
		return 0, err
	}
	if _, duplicate := seen[identity]; duplicate {
		return 0, nil
	}
	seen[identity] = struct{}{}
	return allocatedFileBytes(info)
}
