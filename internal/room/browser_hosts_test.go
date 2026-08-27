package room

import (
	"errors"
	"os"
	"testing"

	"github.com/m1k1o/neko-rooms/internal/config"
	"github.com/m1k1o/neko-rooms/internal/types"
)

func TestBrowserHostKeyIdentifiesBackingProfile(t *testing.T) {
	base := browserHostKey("/accounts/main")
	if got := browserHostKey("/accounts/main/"); got != base {
		t.Fatalf("equivalent paths produced different keys: %q != %q", got, base)
	}
	if got := browserHostKey("/accounts/other"); got == base {
		t.Fatal("different shared paths produced the same browser host key")
	}
}

func TestBrowserHostSpecSeparatesIncompatibleHosts(t *testing.T) {
	profile := browserProfileMount{SharedPath: "/accounts/main", ProfilePath: "/home/neko/profile"}
	settings := types.RoomSettings{
		ApiVersion: 3,
		Screen:     "1280x720@30",
		Envs:       map[string]string{"NEKO_FIREFOX_ARGS": "--private-window"},
		BrowserPolicy: &types.BrowserPolicy{
			Type:    types.FirefoxBrowserPolicy,
			Path:    "/usr/lib/firefox/distribution/policies.json",
			Profile: profile.ProfilePath,
			Content: types.BrowserPolicyContent{PersistentData: true},
		},
	}

	base, err := browserHostSpecHash(settings, profile, "sha256:image-a")
	if err != nil {
		t.Fatal(err)
	}
	same, err := browserHostSpecHash(settings, profile, "sha256:image-a")
	if err != nil || same != base {
		t.Fatalf("equivalent specs produced different hashes: %q != %q (%v)", same, base, err)
	}
	settings.DNS = []string{}
	settings.Resources.Gpus = []string{}
	settings.Resources.Devices = []string{}
	settings.BrowserPolicy.Content.Extensions = []types.BrowserPolicyExtension{}
	normalized, err := browserHostSpecHash(settings, profile, "sha256:image-a")
	if err != nil || normalized != base {
		t.Fatalf("nil and empty collections produced different hashes: %q != %q (%v)", normalized, base, err)
	}

	changedImage, err := browserHostSpecHash(settings, profile, "sha256:image-b")
	if err != nil {
		t.Fatal(err)
	}
	if changedImage == base {
		t.Fatal("different image IDs produced the same browser host spec")
	}

	settings.Screen = "1920x1080@30"
	changedScreen, err := browserHostSpecHash(settings, profile, "sha256:image-a")
	if err != nil {
		t.Fatal(err)
	}
	if changedScreen == base {
		t.Fatal("different screens produced the same browser host spec")
	}
}

func TestSetContainerEnvReplacesExistingValue(t *testing.T) {
	env := setContainerEnv([]string{"A=1", "B=2", "A=old"}, "A", "new")
	want := []string{"B=2", "A=new"}
	if len(env) != len(want) {
		t.Fatalf("environment = %#v, want %#v", env, want)
	}
	for i := range want {
		if env[i] != want[i] {
			t.Fatalf("environment = %#v, want %#v", env, want)
		}
	}
}

func TestBrowserWindowForRegionUsesSlotGeometry(t *testing.T) {
	windows := []browserWindow{
		{ID: 11, X: 2, Y: 2, Width: 1280, Height: 720},
		{ID: 22, X: 1282, Y: 2, Width: 1280, Height: 720},
	}
	host := &BrowserHostLabels{WindowX: 1280, WindowY: 0, WindowWidth: 1280, WindowHeight: 720}
	window, ok := browserWindowForRegion(windows, host, map[uint64]struct{}{})
	if !ok || window.ID != 22 {
		t.Fatalf("window = (%+v, %v), want ID 22", window, ok)
	}
	if _, ok := browserWindowForRegion(windows, host, map[uint64]struct{}{22: {}}); ok {
		t.Fatal("used window was assigned twice")
	}
}

func TestFilterBrowserProcessWindowsIgnoresCrashReporter(t *testing.T) {
	windows := []browserWindow{
		{ID: 1, PID: 100, Process: "firefox-bin", Width: 1280, Height: 720},
		{ID: 2, PID: 100, Process: "firefox-bin", Width: 1280, Height: 720},
		{ID: 3, PID: 200, Process: "crashreporter", Width: 4000, Height: 2000},
	}
	filtered := filterBrowserProcessWindows(windows)
	if len(filtered) != 2 || filtered[0].PID != 100 || filtered[1].PID != 100 {
		t.Fatalf("filtered windows = %#v", filtered)
	}
}

func TestBrowserProfileRequiresMatchingSharedPersistentMount(t *testing.T) {
	manager := &RoomManagerCtx{config: &config.Room{StorageExternal: "/data"}}
	settings := &types.RoomSettings{
		BrowserPolicy: &types.BrowserPolicy{
			Profile: "/home/neko/profile",
			Content: types.BrowserPolicyContent{PersistentData: true},
		},
		Mounts: []types.RoomMount{{
			Type:          types.MountShared,
			HostPath:      "/accounts/main",
			ContainerPath: "/home/neko/profile",
		}},
	}

	profile := manager.browserProfile(settings)
	if profile == nil {
		t.Fatal("matching shared browser profile was not detected")
	}
	if profile.ExternalPath != "/data/shared/accounts/main" {
		t.Fatalf("external path = %q", profile.ExternalPath)
	}

	settings.BrowserPolicy.Content.PersistentData = false
	if profile := manager.browserProfile(settings); profile != nil {
		t.Fatal("non-persistent browser policy produced a browser host")
	}
}

func TestBrowserHostLockIsExclusiveAndOwnerChecked(t *testing.T) {
	lockPath := t.TempDir() + "/profile"
	first := browserHostLockOwner{Version: 1, Key: "profile", DockerID: "docker-a", InstanceName: "rooms-a"}
	second := browserHostLockOwner{Version: 1, Key: "profile", DockerID: "docker-b", InstanceName: "rooms-b"}

	created, err := acquireBrowserHostLockPath(lockPath, first)
	if err != nil || !created {
		t.Fatalf("first lock acquire = (%v, %v), want (true, nil)", created, err)
	}
	created, err = acquireBrowserHostLockPath(lockPath, first)
	if err != nil || created {
		t.Fatalf("owner lock reacquire = (%v, %v), want (false, nil)", created, err)
	}
	if _, err := acquireBrowserHostLockPath(lockPath, second); err == nil {
		t.Fatal("second orchestrator acquired the same browser host lock")
	}
	if err := releaseBrowserHostLockPath(lockPath, second); err == nil {
		t.Fatal("foreign orchestrator released browser host lock")
	}
	if err := releaseBrowserHostLockPath(lockPath, first); err != nil {
		t.Fatalf("owner failed to release browser host lock: %v", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock path still exists after release: %v", err)
	}
}
