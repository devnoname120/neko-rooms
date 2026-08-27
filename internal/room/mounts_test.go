package room

import (
	"testing"

	"github.com/m1k1o/neko-rooms/internal/types"
)

func TestWritableStoragePath(t *testing.T) {
	tests := []struct {
		name     string
		mount    types.MountType
		roomName string
		hostPath string
		wantPath string
		wantOK   bool
	}{
		{
			name:     "private mount is scoped to room",
			mount:    types.MountPrivate,
			roomName: "example",
			hostPath: "/profile",
			wantPath: "rooms/example/profile",
			wantOK:   true,
		},
		{
			name:     "shared mount is independent of room",
			mount:    types.MountShared,
			roomName: "example",
			hostPath: "/home/neko/.config/chromium",
			wantPath: "shared/home/neko/.config/chromium",
			wantOK:   true,
		},
		{
			name:     "template mount is not writable",
			mount:    types.MountTemplate,
			roomName: "example",
			hostPath: "/policy.json",
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPath, gotOK := writableStoragePath(tt.mount, tt.roomName, tt.hostPath)
			if gotOK != tt.wantOK {
				t.Fatalf("writableStoragePath() ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotPath != tt.wantPath {
				t.Fatalf("writableStoragePath() path = %q, want %q", gotPath, tt.wantPath)
			}
		})
	}
}

func TestRelativeMountPath(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		root     string
		wantPath string
		wantOK   bool
	}{
		{
			name:     "nested path",
			source:   "/data/shared/home/neko/.config/chromium",
			root:     "/data/shared",
			wantPath: "/home/neko/.config/chromium",
			wantOK:   true,
		},
		{
			name:     "storage root",
			source:   "/data/shared",
			root:     "/data/shared",
			wantPath: "/",
			wantOK:   true,
		},
		{
			name:   "similar prefix",
			source: "/data/shared-other/profile",
			root:   "/data/shared",
			wantOK: false,
		},
		{
			name:   "outside root",
			source: "/data/rooms/example/profile",
			root:   "/data/shared",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPath, gotOK := relativeMountPath(tt.source, tt.root)
			if gotOK != tt.wantOK {
				t.Fatalf("relativeMountPath() ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotPath != tt.wantPath {
				t.Fatalf("relativeMountPath() path = %q, want %q", gotPath, tt.wantPath)
			}
		})
	}
}
