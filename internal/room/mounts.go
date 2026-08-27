package room

import (
	"path"
	"path/filepath"
	"strings"

	"github.com/m1k1o/neko-rooms/internal/types"
)

func writableStoragePath(mountType types.MountType, roomName, hostPath string) (string, bool) {
	switch mountType {
	case types.MountPrivate:
		return path.Join(privateStoragePath, roomName, hostPath), true
	case types.MountShared:
		return path.Join(sharedStoragePath, hostPath), true
	default:
		return "", false
	}
}

// relativeMountPath returns an API mount path when source is inside root.
// The separator check prevents similarly prefixed host paths from being
// mistaken for managed storage (for example, /data/shared-other).
func relativeMountPath(source, root string) (string, bool) {
	source = filepath.Clean(source)
	root = filepath.Clean(root)

	if source == root {
		return "/", true
	}

	prefix := root + string(filepath.Separator)
	if !strings.HasPrefix(source, prefix) {
		return "", false
	}

	return "/" + filepath.ToSlash(strings.TrimPrefix(source, prefix)), true
}
