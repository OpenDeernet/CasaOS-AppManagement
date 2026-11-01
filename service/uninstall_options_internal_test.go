package service

import "testing"

func TestAnUninstallKeepsTheNamedVolumesUnlessTheDataIsDeleted(t *testing.T) {
	if got := uninstallDownOptions(false); got.Volumes {
		t.Fatalf("an uninstall that keeps the data asked compose to remove the volumes: %+v", got)
	}

	got := uninstallDownOptions(true)
	if !got.Volumes {
		t.Fatalf("an uninstall that deletes the data kept the volumes: %+v", got)
	}

	for _, keep := range []bool{false, true} {
		if got := uninstallDownOptions(keep); !got.RemoveOrphans || got.Images != "all" {
			t.Fatalf("the rest of an uninstall changed with the data (%v): %+v", keep, got)
		}
	}
}
