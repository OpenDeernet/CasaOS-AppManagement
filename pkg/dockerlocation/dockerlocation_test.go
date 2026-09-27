package dockerlocation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func host(t *testing.T) *Mover {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"/var/lib/docker/volumes", "/var/lib/containerd/io", "/DATA/big", "/DATA/used/docker", "/etc/systemd/system", "/var/lib/casaos"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "/DATA/used/docker/x"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "/etc/fstab"), []byte("UUID=1 / ext4 defaults 0 1\n"), 0o644)

	m := &Mover{
		Root:      root,
		StatePath: filepath.Join(root, "/var/lib/casaos/docker-location.json"),
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			return "", nil
		},
		StatFS: func(path string) (string, int64, error) { return "ext4", 100 << 30, nil },
	}
	m.load()
	return m
}

func TestCheckTarget(t *testing.T) {
	m := host(t)
	ctx := context.Background()

	if c := m.CheckTarget(ctx, "/DATA/big"); !c.OK || c.FSType != "ext4" {
		t.Errorf("a good folder: %+v", c)
	}

	for path, why := range map[string]string{
		"/etc":          "under",
		"/DATA/missing": "does not exist",
		"/DATA/used":    "not empty",
		"/DATA/a b":     "letters",
		"relative":      "letters",
	} {
		if c := m.CheckTarget(ctx, path); c.OK || !strings.Contains(c.Reason, why) {
			t.Errorf("%s: %+v, want %q", path, c, why)
		}
	}

	m.StatFS = func(string) (string, int64, error) { return "0x65735546", 100 << 30, nil }
	if c := m.CheckTarget(ctx, "/DATA/big"); c.OK || !strings.Contains(c.Reason, "ext4, xfs or btrfs") {
		t.Errorf("a FUSE drive: %+v", c)
	}

	m.StatFS = func(string) (string, int64, error) { return "ext4", 1, nil }
	if c := m.CheckTarget(ctx, "/DATA/big"); c.OK || !strings.Contains(c.Reason, "free") {
		t.Errorf("a full drive: %+v", c)
	}
}

func TestUnits(t *testing.T) {
	u := MountUnit("/media/big/docker", "/var/lib/docker", "/media/big", "docker.service")
	for _, want := range []string{"What=/media/big/docker", "Where=/var/lib/docker", "Options=bind", "RequiresMountsFor=/media/big", "Before=docker.service", "WantedBy=local-fs.target"} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
	if unitValue(u, "What") != "/media/big/docker" {
		t.Errorf("What %q", unitValue(u, "What"))
	}
	if !strings.Contains(DropIn("/var/lib/docker"), "RequiresMountsFor=/var/lib/docker") {
		t.Error("drop-in")
	}
}

func TestLocationFromUnit(t *testing.T) {
	m := host(t)
	if m.State().Location != "" {
		t.Errorf("default place: %q", m.State().Location)
	}
	os.WriteFile(m.path(unitPath("var-lib-docker.mount")), []byte(MountUnit("/media/big/docker", "/var/lib/docker", "/media/big", "docker.service")), 0o644)
	m.load()
	if m.State().Location != "/media/big" {
		t.Errorf("moved: %q", m.State().Location)
	}
}

func TestInterruptedMoveIsReported(t *testing.T) {
	m := host(t)
	os.WriteFile(m.StatePath, []byte(`{"phase":"copying","target":"/DATA/big"}`), 0o644)
	m.load()
	if s := m.State(); s.Phase != PhaseFailed || !strings.Contains(s.Message, "interrupted") {
		t.Errorf("%+v", s)
	}
}

func TestParseProgress(t *testing.T) {
	cases := map[string]int64{
		"      1,234,567  12%   10.00MB/s    0:00:10 (xfr#3, to-chk=10/20)": 1234567,
		"             32 100%    0.00kB/s    0:00:00":                       32,
		"sending incremental file list":                                     0,
	}
	for line, want := range cases {
		if got := ParseProgress(line); got != want {
			t.Errorf("%q: %d, want %d", line, got, want)
		}
	}
}

func TestSwitchFromDefaultAndUndo(t *testing.T) {
	m := host(t)
	ctx := context.Background()
	target := "/DATA/big"
	for _, a := range areas {
		os.MkdirAll(m.path(filepath.Join(target, a.name)), 0o711)
	}
	os.WriteFile(m.path("/var/lib/docker/volumes/marker"), []byte("old"), 0o644)

	undo, err := m.switchMounts(ctx, "", target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.path("/var/lib/docker.casaos-old/volumes/marker")); err != nil {
		t.Error("the old data was not set aside")
	}
	if b, _ := os.ReadFile(m.path(unitPath("var-lib-docker.mount"))); !strings.Contains(string(b), "What=/DATA/big/docker") {
		t.Errorf("unit:\n%s", b)
	}
	if _, err := os.Stat(m.path(dropInPath("containerd.service"))); err != nil {
		t.Error("no drop-in for containerd")
	}
	if got := m.oldCopy(""); len(got) != 2 {
		t.Errorf("old copy %v", got)
	}

	undo()
	if b, err := os.ReadFile(m.path("/var/lib/docker/volumes/marker")); err != nil || string(b) != "old" {
		t.Error("undo did not put the data back")
	}
	if _, err := os.Stat(m.path(unitPath("var-lib-docker.mount"))); !os.IsNotExist(err) {
		t.Error("undo left the unit")
	}
	if _, err := os.Stat(m.path("/var/lib/docker.casaos-old")); !os.IsNotExist(err) {
		t.Error("undo left the aside folder")
	}
}

func TestDeleteOldCopyRefusesLiveFolders(t *testing.T) {
	m := host(t)
	m.state.OldCopy = []string{"/var/lib/docker"}
	if _, err := m.DeleteOldCopy(); err == nil {
		t.Error("deleted the live folder")
	}
	os.MkdirAll(m.path("/var/lib/docker.casaos-old/x"), 0o755)
	m.state.OldCopy = []string{"/var/lib/docker.casaos-old"}
	if _, err := m.DeleteOldCopy(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.path("/var/lib/docker.casaos-old")); !os.IsNotExist(err) {
		t.Error("the old copy stayed")
	}
}

func TestMountsUnderAndUnmount(t *testing.T) {
	m := host(t)
	os.MkdirAll(m.path("/proc/self"), 0o755)
	mountinfo := `33 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw
655 33 259:2 /DATA/x/docker /var/lib/docker rw - ext4 /dev/nvme0n1p2 rw
933 655 0:69 / /var/lib/docker/rootfs/overlayfs/abc rw - overlay overlay rw
934 933 0:70 / /var/lib/docker/rootfs/overlayfs/abc/deeper rw - overlay overlay rw
935 33 0:71 / /var/lib/dockerish rw - tmpfs tmpfs rw
`
	os.WriteFile(m.path("/proc/self/mountinfo"), []byte(mountinfo), 0o644)

	got := m.mountsUnder("/var/lib/docker", false)
	want := []string{"/var/lib/docker/rootfs/overlayfs/abc/deeper", "/var/lib/docker/rootfs/overlayfs/abc"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("under: %v", got)
	}
	if got := m.mountsUnder("/var/lib/docker", true); len(got) != 3 {
		t.Errorf("with itself: %v", got)
	}

	var unmounted []string
	m.Run = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "umount" {
			unmounted = append(unmounted, args[0])
		}
		return "", nil
	}
	// the fake mountinfo does not change, so the check afterwards fails
	if err := m.unmountUnder(context.Background(), "/var/lib/docker", false); err == nil || len(unmounted) != 2 || unmounted[0] != want[0] {
		t.Errorf("unmount order %v, err %v", unmounted, err)
	}
}

func TestBackToDefault(t *testing.T) {
	m := host(t)
	ctx := context.Background()

	if c := m.CheckTarget(ctx, Default); c.OK {
		t.Error("already in the default place, yet OK")
	}

	// moved to /DATA/big: units written, live folders empty mount points
	os.MkdirAll(m.path("/DATA/big/docker"), 0o711)
	os.MkdirAll(m.path("/DATA/big/containerd"), 0o711)
	undo, err := m.switchMounts(ctx, "", "/DATA/big")
	if err != nil {
		t.Fatal(err)
	}
	_ = undo
	m.load()
	if m.State().Location != "/DATA/big" {
		t.Fatalf("location %q", m.State().Location)
	}
	if c := m.CheckTarget(ctx, Default); !c.OK {
		t.Fatalf("back: %+v", c)
	}

	// the copy back is next to the live folders
	for _, a := range areas {
		os.MkdirAll(m.path(a.live+newSuffix), 0o711)
		os.WriteFile(m.path(a.live+newSuffix+"/marker"), []byte("new"), 0o644)
	}

	undo, err = m.switchToDefault(ctx, "/DATA/big", Default)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(m.path("/var/lib/docker/marker")); string(b) != "new" {
		t.Error("the copy did not take the live folder's place")
	}
	if _, err := os.Stat(m.path(unitPath("var-lib-docker.mount"))); !os.IsNotExist(err) {
		t.Error("the unit stayed")
	}
	if m.location() != "" {
		t.Errorf("location %q", m.location())
	}

	undo()
	if _, err := os.Stat(m.path(unitPath("var-lib-docker.mount"))); err != nil {
		t.Error("undo did not put the unit back")
	}
	if b, _ := os.ReadFile(m.path("/var/lib/docker.casaos-new/marker")); string(b) != "new" {
		t.Error("undo did not set the copy aside again")
	}
}
