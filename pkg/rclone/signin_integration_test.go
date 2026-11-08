package rclone

import (
	"os"
	"strings"
	"testing"
)

// Run against a real rclone: SIGNIN_RCLONE=/path/to/rclone go test -run RealRclone.
// It goes as far as the provider's address; signing in for real takes a person.
func TestSignInWithARealRclone(t *testing.T) {
	binary := os.Getenv("SIGNIN_RCLONE")
	if binary == "" {
		t.Skip("SIGNIN_RCLONE is not set")
	}
	previous := rcloneBinary
	rcloneBinary = binary
	t.Cleanup(func() {
		currentMu.Lock()
		stopCurrent()
		currentMu.Unlock()
		rcloneBinary = previous
	})

	first, err := SignIn("drive")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "https://accounts.google.com/") || !strings.Contains(first, "redirect_uri=http%3A%2F%2F127.0.0.1%3A53682%2F") {
		t.Fatalf("not Google's sign-in for the loopback redirect: %s", first)
	}

	// a wrong state is refused here: sent on, it would end rclone's sign-in
	if _, _, err := CompleteSignIn("http://127.0.0.1:53682/?state=wrong&code=c"); err == nil || !strings.Contains(err.Error(), "another sign-in") {
		t.Fatalf("want a refusal, got %v", err)
	}

	// a second sign-in replaces the first and gets the port
	second, err := SignIn("dropbox")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(second, "https://www.dropbox.com/") {
		t.Fatalf("not Dropbox's sign-in: %s", second)
	}

	third, err := SignIn("onedrive")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(third, "https://login.microsoftonline.com/") || !strings.Contains(third, "redirect_uri=http%3A%2F%2Flocalhost%3A53682%2F") {
		t.Fatalf("not Microsoft's sign-in for the loopback redirect: %s", third)
	}
}
