// Package rclone talks to the rclone daemon this distribution already installs.
//
// The installer puts rclone on every box and runs it as a service:
//
//	ExecStart=/usr/bin/rclone rcd --rc-addr unix:///var/run/rclone/rclone.sock --rc-no-auth
//
// which means S3, SFTP, FTP, the retries, the resume after a broken connection and
// the incremental comparison are already here, already running, and already driven
// over an API. None of that is worth writing again in Go.
//
// Two things about that daemon are worth knowing before trusting it with
// credentials. It listens with --rc-no-auth, so any local process can read and
// write every remote it holds; that is already true of the cloud drives CasaOS
// stores there today. And it is pinned at v1.61.1, which is old but long predates
// every backend used here.
package rclone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
)

// SocketPath is where the unit file puts the daemon's socket.
const SocketPath = "/var/run/rclone/rclone.sock"

// RawSuffix marks the remote underneath a destination that has a root or is
// encrypted: the bucket or the server itself (which, when encrypted, holds only
// ciphertext), never listed as a destination in its own right. A name ending in it
// is refused, so the two can always be told apart.
const RawSuffix = "-raw"

// DestinationPrefix marks a remote as one of ours.
//
// Backup destinations live in rclone's own config, next to the cloud drives
// CasaOS mounts -- which is deliberate: the credentials then sit where this
// project already keeps credentials of exactly that kind, rather than in a second
// store invented for the purpose. The prefix is what tells the two apart, and it
// is readable in `rclone config` by anyone wondering what CasaOS put there.
const DestinationPrefix = "casaos-backup-"

// ErrDaemonUnreachable is returned when the socket is not there or not answering.
// It is worth its own error: it means rclone is not running, which is a different
// problem from a destination being misconfigured, and the two get different advice.
var ErrDaemonUnreachable = errors.New("the rclone daemon is not answering on " + SocketPath)

// Client drives the rclone daemon over its unix socket.
type Client struct {
	http *resty.Client
}

// NewClient returns a client bound to the daemon's socket.
//
// The timeout here covers control calls only -- listing remotes, creating one,
// checking one answers. A transfer is started as a job and polled, so no copy ever
// has to finish inside one request.
func NewClient() *Client {
	transport := &http.Transport{
		Dial: func(_, _ string) (net.Conn, error) {
			return net.Dial("unix", SocketPath)
		},
	}

	http := resty.New().
		SetTransport(transport).
		SetBaseURL("http://localhost").
		SetTimeout(30 * time.Second)

	return &Client{http: http}
}

// call posts to one of the daemon's rc endpoints.
//
// Parameters go as form data rather than JSON because that is what the daemon
// accepts for these, and because it keeps a credential out of a JSON body that
// would otherwise be easy to log whole.
func (c *Client) call(endpoint string, params map[string]string, out interface{}) error {
	return c.callContext(context.Background(), endpoint, params, out)
}

// callContext is call, given up when ctx is: the caller then hears its own
// cancellation, not a daemon that looks gone.
func (c *Client) callContext(ctx context.Context, endpoint string, params map[string]string, out interface{}) error {
	response, err := c.http.R().SetContext(ctx).SetFormData(params).Post(endpoint)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A socket that is not there is the common case, and it reads as a
		// connection error rather than anything rclone said.
		return fmt.Errorf("%w: %w", ErrDaemonUnreachable, err)
	}

	if response.StatusCode() != http.StatusOK {
		// The daemon answers errors as {"error": "..."} and the message is the
		// useful half -- the status alone says nothing a person can act on.
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(response.Body(), &failure) == nil && failure.Error != "" {
			return answeredError{fmt.Errorf("rclone: %s", failure.Error)}
		}

		return answeredError{fmt.Errorf("rclone answered %d to %s", response.StatusCode(), endpoint)}
	}

	if out == nil {
		return nil
	}

	return json.Unmarshal(response.Body(), out)
}

// answeredError is the daemon refusing a call, as opposed to the call not
// reaching it or its answer being lost: whatever was asked did not happen.
type answeredError struct{ error }

// notStarted says a failed call certainly did nothing: the daemon refused it, or
// the connection to it was never made.
func notStarted(err error) bool {
	var dial *net.OpError

	return errors.As(err, &answeredError{}) || (errors.As(err, &dial) && dial.Op == "dial")
}

// Destinations lists the remotes this project put there, by their short name --
// the prefix is an implementation detail and does not belong in an interface.
func (c *Client) Destinations() ([]string, error) {
	var result struct {
		Remotes []string `json:"remotes"`
	}

	if err := c.call("/config/listremotes", nil, &result); err != nil {
		return nil, err
	}

	names := []string{}
	for _, remote := range result.Remotes {
		if short, ok := strings.CutPrefix(remote, DestinationPrefix); ok && !strings.HasSuffix(short, RawSuffix) {
			names = append(names, short)
		}
	}

	return names, nil
}

// CreateDestination adds or replaces a backup destination.
//
// `backend` is an rclone backend name -- "s3", "sftp", "ftp" -- and `parameters`
// are that backend's own options. Neither is interpreted here: rclone validates
// them, rclone knows what each backend needs, and a list of required fields
// maintained on this side would go stale the first time rclone gained an option.
//
// `root` is where on the backend everything is put, handed to rclone as it is: a
// folder, or for an object store the bucket (rclone has no bucket option; the
// first folder of a path is the bucket, which left to itself is named after the
// app, and a name at a provider such as AWS belongs to whoever took it first).
// Empty, the destination is the backend's own root.
func (c *Client) CreateDestination(name, backend, root string, parameters map[string]string) error {
	if err := checkDestination(name, backend); err != nil {
		return err
	}

	root = strings.TrimSpace(root)
	if root == "" {
		return c.createRemote(name, backend, parameters)
	}

	if err := c.createRemote(name+RawSuffix, backend, parameters); err != nil {
		return err
	}

	// An alias is a name for a place inside another remote: everything else in this
	// package talks to it and never knows the difference.
	if err := c.createRemoteOf(name, "alias", map[string]string{"remote": rawRemote(name, root)}, nil); err != nil {
		_ = c.DeleteDestination(name)

		return err
	}

	return nil
}

func checkDestination(name, backend string) error {
	if name == "" {
		return errors.New("a destination needs a name")
	}
	if strings.HasSuffix(name, RawSuffix) {
		return fmt.Errorf("a destination's name cannot end in %q", RawSuffix)
	}
	if backend == "" {
		return errors.New("a destination needs a backend, such as s3, sftp or ftp")
	}

	return nil
}

// rawRemote is the root on the remote underneath the destination called name.
func rawRemote(name, root string) string {
	return DestinationPrefix + name + RawSuffix + ":" + root
}

func (c *Client) createRemote(name, backend string, parameters map[string]string) error {
	// A backend that signs in through a browser (onedrive, drive, dropbox) is given
	// the token rclone authorize printed elsewhere. Asked to refresh it, which is
	// its default, rclone starts its own sign-in and waits for a browser this box
	// does not have, and the call times out: keep the token as it is given.
	if parameters["token"] != "" {
		if _, set := parameters["config_refresh_token"]; !set {
			parameters = maps.Clone(parameters)
			parameters["config_refresh_token"] = "false"
		}
	}

	return c.createRemoteOf(name, backend, parameters, nil)
}

// createRemoteOf asks rclone for the remote called name (without the prefix).
func (c *Client) createRemoteOf(name, backend string, parameters map[string]string, extra map[string]string) error {
	encoded, err := json.Marshal(parameters)
	if err != nil {
		return err
	}

	call := map[string]string{
		"name":       DestinationPrefix + name,
		"type":       backend,
		"parameters": string(encoded),
	}
	maps.Copy(call, extra)

	return c.call("/config/create", call, nil)
}

// DeleteDestination forgets a destination and its credentials. Whatever was
// already copied to it stays where it is: this removes the way in, not the backup.
func (c *Client) DeleteDestination(name string) error {
	if err := c.call("/config/delete", map[string]string{
		"name": DestinationPrefix + name,
	}, nil); err != nil {
		return err
	}

	// The remote underneath an encrypted destination goes with it. Deleting a
	// remote that does not exist is not an error worth reporting: a plain
	// destination has none.
	_ = c.call("/config/delete", map[string]string{
		"name": DestinationPrefix + name + RawSuffix,
	}, nil)

	return nil
}

// DestinationSpace is what a destination says about itself when asked.
type DestinationSpace struct {
	// Used and Free are bytes, and either may be absent: plenty of backends,
	// object stores especially, have no answer to "how much room is left", and
	// inventing one would be worse than saying nothing.
	Used  *int64 `json:"used,omitempty"`
	Free  *int64 `json:"free,omitempty"`
	Total *int64 `json:"total,omitempty"`
}

// CheckDestination asks a destination whether it is reachable, and what it can say
// about its space.
//
// Worth doing at the moment somebody configures a destination rather than at the
// moment a backup runs: a typo in a bucket name found at 3am by a scheduled job
// nobody is watching is a backup that never existed.
func (c *Client) CheckDestination(name string) (DestinationSpace, error) {
	var space DestinationSpace

	fs := DestinationPrefix + name + ":"

	err := c.call("/operations/about", map[string]string{"fs": fs}, &space)
	if err != nil && strings.Contains(err.Error(), "doesn't support about") {
		// S3 answers an error here, not an empty space: it has no About at all. Whether the
		// destination can be reached is then what listing its root says, and no space is told.
		err = c.call("/operations/list", map[string]string{"fs": fs, "remote": ""}, nil)
		if err != nil && strings.Contains(err.Error(), "directory not found") {
			// a root typed by hand: this is the typo, or the bucket not made yet
			err = fmt.Errorf("the bucket or folder of this destination does not exist (%w)", err)
		}

		return DestinationSpace{}, err
	}

	return space, err
}

// CreateEncryptedDestination makes a destination whose contents -- names and
// bytes both -- are encrypted before they leave this box.
//
// Two remotes: the backend itself under the raw name, and rclone's crypt backend
// on top of it under the destination's name. Everything else in this package
// talks to the top one and never knows the difference; a bucket at a provider
// holds ciphertext it cannot read. The password is obscured by rclone into its
// config the way it obscures every password it keeps, and is not readable back
// through this API.
func (c *Client) CreateEncryptedDestination(name, backend, root string, parameters map[string]string, password string) error {
	if err := checkDestination(name, backend); err != nil {
		return err
	}
	if password == "" {
		return errors.New("an encrypted destination needs a password")
	}

	if err := c.createRemote(name+RawSuffix, backend, parameters); err != nil {
		return err
	}

	encoded, err := json.Marshal(map[string]string{
		"remote":   rawRemote(name, strings.TrimSpace(root)),
		"password": password,
	})
	if err != nil {
		return err
	}

	if err := c.call("/config/create", map[string]string{
		"name":       DestinationPrefix + name,
		"type":       "crypt",
		"parameters": string(encoded),
		"opt":        `{"obscure": true}`,
	}, nil); err != nil {
		// half a destination is worse than none: the raw remote alone would be
		// listed by nothing and reachable by anything
		_ = c.DeleteDestination(name)

		return err
	}

	return nil
}
