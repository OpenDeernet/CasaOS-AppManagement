package rclone

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Signing in to a cloud drive from the interface.
//
// A box has no browser, so rclone's own sign-in has nowhere to open. `rclone
// authorize` is that same sign-in with the browser left out: it listens on
// 127.0.0.1:53682 of the box, prints a link, and prints the token once the provider
// has redirected back to it. This runs it on the box and carries the two halves
// the person's browser cannot do itself:
//
//  1. SignIn starts it and gives back the provider's own address to open.
//  2. The provider then redirects to 127.0.0.1:53682 -- on the person's computer,
//     where nothing listens, so that page fails. The address of that failed page
//     holds everything rclone needs, and CompleteSignIn replays it on the box.
//
// The redirect cannot be pointed at the box instead: the providers accept plain
// http redirects to the loopback only, and rclone's public client is registered for
// exactly this address.
const signInAddr = "127.0.0.1:53682"

// signInBackends are the drives whose token rclone authorize hands over whole.
// OneDrive also needs a drive_id and a drive_type, which only its own configuration
// finds out, so it keeps the longer instructions.
var signInBackends = []string{"drive", "dropbox"}

// ErrNoSignIn is what completing a sign-in says when none is waiting: it was never
// started, it timed out, or a newer one took its place.
var ErrNoSignIn = BadSignIn("no sign-in is waiting: start it again")

// BadSignIn is a sign-in error that comes from what was asked or pasted, not from
// rclone or the box: the person can fix it, so it is reported as a bad request.
type BadSignIn string

func (e BadSignIn) Error() string { return string(e) }

var (
	rcloneBinary = "rclone"
	signInLife   = 10 * time.Minute // how long the box waits for the person
	signInStart  = 15 * time.Second // how long rclone has to print its link
	signInFinish = 30 * time.Second // how long rclone has to trade the code for a token
)

// authLink is the address rclone prints to open: its own page on the box, which
// redirects to the provider.
var authLink = regexp.MustCompile(`https?://\S+/auth\?state=\S+`)

type signIn struct {
	state  string
	cancel context.CancelFunc
	exited chan struct{}
	token  chan string
	mu     sync.Mutex
	output bytes.Buffer
}

// One at a time: rclone listens on a fixed port, so a second sign-in could not
// start until the first let go of it.
var (
	currentMu sync.Mutex
	current   *signIn
)

// SignInBackends lists the backends SignIn handles.
func SignInBackends() []string { return slices.Clone(signInBackends) }

// SignIn starts a sign-in to backend and returns the address to open in a browser.
// It replaces any sign-in already waiting.
func SignIn(backend string) (string, error) {
	if !slices.Contains(signInBackends, backend) {
		return "", BadSignIn(fmt.Sprintf("signing in from the interface is not offered for %q", backend))
	}

	currentMu.Lock()
	defer currentMu.Unlock()

	stopCurrent()

	ctx, cancel := context.WithTimeout(context.Background(), signInLife)
	cmd := exec.CommandContext(ctx, rcloneBinary, "authorize", backend, "--auth-no-open-browser")
	reader, writer := io.Pipe()
	cmd.Stdout = writer
	cmd.Stderr = writer

	if err := cmd.Start(); err != nil {
		cancel()

		return "", fmt.Errorf("%w: %s", ErrDaemonUnreachable, err)
	}

	s := &signIn{cancel: cancel, exited: make(chan struct{}), token: make(chan string, 1)}
	link := make(chan string, 1)

	go func() {
		_ = cmd.Wait()
		_ = writer.Close()
		close(s.exited)
	}()
	go s.read(reader, link)

	select {
	case address := <-link:
		provider, state, err := providerAddress(address)
		if err != nil {
			s.stop()

			return "", err
		}
		s.state = state
		current = s

		return provider, nil
	case <-s.exited:
		return "", s.failure("rclone stopped before it gave a link")
	case <-time.After(signInStart):
		s.stop()

		return "", errors.New("rclone gave no link to sign in with")
	}
}

// CompleteSignIn takes the address of the page the provider redirected to -- the
// one that failed to load -- and returns the token rclone then produced.
func CompleteSignIn(redirected string) (string, error) {
	currentMu.Lock()
	s := current
	currentMu.Unlock()

	if s == nil {
		return "", ErrNoSignIn
	}

	query, err := redirectQuery(redirected, s.state)
	if err != nil {
		return "", err
	}

	// Only the query is replayed, onto the address rclone listens on: what was
	// pasted never decides where this box connects.
	response, err := http.Get("http://" + signInAddr + "/?" + query) //nolint:gosec,noctx // fixed loopback address
	if err != nil {
		return "", ErrNoSignIn
	}
	_ = response.Body.Close()

	select {
	case token := <-s.token:
		return token, nil
	case <-s.exited:
		return "", s.failure("rclone stopped before it gave a token")
	case <-time.After(signInFinish):
		return "", errors.New("rclone did not give a token: start the sign-in again")
	}
}

// stopCurrent ends the sign-in in progress and waits for rclone to let go of its
// port. The caller holds currentMu.
func stopCurrent() {
	if current != nil {
		current.stop()
		current = nil
	}
}

func (s *signIn) stop() {
	s.cancel()
	<-s.exited
}

// failure explains why rclone ended, with the tail of what it said.
func (s *signIn) failure(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// rclone ends a failure with its usage text, which hides the cause; the cause
	// is in the "Fatal error" it prints last.
	said := strings.TrimSpace(s.output.String())
	if i := strings.LastIndex(said, "Fatal error:"); i >= 0 {
		said = said[i:]
		if len(said) > 300 {
			said = said[:300]
		}
	} else if len(said) > 300 {
		said = said[len(said)-300:]
	}
	if said == "" {
		return errors.New(reason)
	}

	return fmt.Errorf("%s: %s", reason, said)
}

// read follows rclone's output for the link it prints and the token that ends it.
func (s *signIn) read(reader io.Reader, link chan<- string) {
	var inToken bool
	var token []string

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case inToken && strings.Contains(line, "<---End paste"):
			s.token <- strings.TrimSpace(strings.Join(token, "\n"))
			inToken = false
		case inToken:
			token = append(token, line)
		case strings.Contains(line, "Paste the following"):
			// The token itself is not kept in output: a failure reports that.
			inToken = true
		default:
			s.mu.Lock()
			s.output.WriteString(line + "\n")
			s.mu.Unlock()

			if found := authLink.FindString(line); found != "" {
				select {
				case link <- found:
				default:
				}
			}
		}
	}
}

// providerAddress asks rclone's page on the box where it redirects to, which is the
// provider's sign-in, and returns it with the state rclone will insist on.
func providerAddress(local string) (address, state string, err error) {
	parsed, err := url.Parse(local)
	if err != nil {
		return "", "", err
	}
	state = parsed.Query().Get("state")

	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// rclone prints the link just before it serves, so a first refusal is not final.
	var response *http.Response
	for range 5 {
		response, err = client.Get("http://" + signInAddr + parsed.RequestURI()) //nolint:noctx // fixed loopback address
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		return "", "", fmt.Errorf("rclone's sign-in page does not answer: %w", err)
	}
	_ = response.Body.Close()

	address = response.Header.Get("Location")
	if state == "" || !strings.HasPrefix(address, "https://") {
		return "", "", errors.New("rclone's sign-in page did not point to a provider")
	}

	return address, state, nil
}

// redirectQuery checks what was pasted and returns its query.
//
// The person may paste the whole address, or only what follows the question mark.
// Either way it has to carry the code and the state of the sign-in waiting: an
// address left over from another attempt would be refused by rclone anyway, but
// saying so here is clearer than a timeout.
func redirectQuery(pasted, state string) (string, error) {
	pasted = strings.TrimSpace(pasted)
	if _, query, found := strings.Cut(pasted, "?"); found {
		pasted = query
	}

	values, err := url.ParseQuery(pasted)
	if err != nil {
		return "", BadSignIn("that is not the address of the page that failed to load")
	}
	if denied := values.Get("error"); denied != "" {
		return "", BadSignIn("the provider refused the sign-in: " + denied)
	}
	if values.Get("code") == "" || values.Get("state") == "" {
		return "", BadSignIn("that address has no code in it: paste the whole address of the page that failed to load")
	}
	if values.Get("state") != state {
		return "", BadSignIn("that address is from another sign-in: start it again and use the new one")
	}

	return values.Encode(), nil
}
