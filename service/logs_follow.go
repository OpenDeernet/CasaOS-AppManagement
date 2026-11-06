package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/logstore"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"go.uber.org/zap"
)

// A follower reads one container's log stream into the store, from where its files stop,
// until the stream ends with the container.

// sinceFor is where a follower resumes: after last, but never before floor, the start of the
// retention (zero: none). Unlike the spec's "catch up on everything", a first follow does not
// ingest what Docker holds from before the retention, only for the purge to remove it.
// Docker's Since is inclusive at second granularity on some versions, so the follower also
// drops the lines at or before last.
func sinceFor(last time.Time, ok bool, floor time.Time) string {
	since := floor
	if ok && last.Add(time.Nanosecond).After(since) {
		since = last.Add(time.Nanosecond)
	}
	if since.IsZero() {
		return ""
	}
	return since.UTC().Format(time.RFC3339Nano)
}

// splitDockerLine splits "2026-11-05T10:00:00.123456789Z text", as Docker's Timestamps give it.
func splitDockerLine(b []byte) (at time.Time, text string, ok bool) {
	ts, rest, _ := bytes.Cut(b, []byte(" "))
	at, err := time.Parse(time.RFC3339Nano, string(ts))
	if err != nil {
		return time.Time{}, "", false
	}
	return at, string(rest), true
}

// FollowedContainers is the ids of the containers a follower reads right now.
func FollowedContainers() map[string]bool {
	followMu.Lock()
	defer followMu.Unlock()
	ids := make(map[string]bool, len(followers))
	for id := range followers {
		ids[id] = true
	}
	return ids
}

// follow starts the container's follower unless it has one. A start (restart true) for a
// container whose last run still drains is followed by the same goroutine once that run ends.
func follow(ctx context.Context, docker logsDocker, id, app, service, name string, restart bool) {
	key := shortID(id)
	followMu.Lock()
	defer followMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if f, running := followers[key]; running {
		f.again = f.again || restart
		return
	}
	fctx, cancel := context.WithCancel(ctx)
	f := &follower{cancel: cancel, done: make(chan struct{}), app: app, service: service, name: strings.TrimPrefix(name, "/")}
	followers[key] = f
	go func() {
		defer close(f.done)
		defer cancel()
		for {
			readLogs(fctx, docker, id, key, app, service)
			followMu.Lock()
			again := f.again && fctx.Err() == nil
			f.again = false
			if !again {
				delete(followers, key)
			}
			followMu.Unlock()
			if !again {
				return
			}
		}
	}()
}

// awaitFollower waits up to grace for the container's follower to end. It never cuts the
// stream: a dead container's stream ends by itself, and its last lines are its crash reason.
func awaitFollower(id string, grace time.Duration) {
	followMu.Lock()
	f := followers[id]
	followMu.Unlock()
	if f == nil {
		return
	}
	select {
	case <-f.done:
	case <-time.After(grace):
	}
}

// stopFollowers ends the followers match accepts (nil: all) and waits for them.
func stopFollowers(match func(*follower) bool) {
	followMu.Lock()
	var stopping []*follower
	for _, f := range followers {
		if match == nil || match(f) {
			f.cancel()
			stopping = append(stopping, f)
		}
	}
	followMu.Unlock()
	for _, f := range stopping {
		<-f.done
	}
}

func readLogs(ctx context.Context, docker logsDocker, id, key, app, service string) {
	store := Logs()
	if store == nil {
		return
	}
	defer func() {
		if err := store.CloseWriter(key); err != nil {
			logger.Error("a container's last logs could not be written", zap.Error(err), zap.String("container", key))
		}
	}()

	info, err := docker.ContainerInspect(ctx, id)
	if err != nil {
		logFollowError(ctx, key, err)
		return
	}
	last, hasLast := logstore.LastTimestamp(store.Root, app, service, key)
	var floor time.Time
	if days, _, _ := logsConfig(); days > 0 {
		floor = time.Now().AddDate(0, 0, -days)
	}
	body, err := docker.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Follow: true, Timestamps: true, Since: sinceFor(last, hasLast, floor),
	})
	if err != nil {
		logFollowError(ctx, key, err)
		return
	}
	defer body.Close()

	write := func(stream string) func([]byte) {
		return func(b []byte) {
			at, text, ok := splitDockerLine(b)
			if !ok || hasLast && !at.After(last) {
				return
			}
			// the writer is looked up per line: a die's CloseWriter must not leave this one closed
			_ = store.Writer(app, service, key).Write(at, stream, text) // errors go to OnError
		}
	}

	if info.Config != nil && info.Config.Tty {
		scanLines(body, write("o"))
		return
	}
	if err := readFrames(body, write("o"), write("e")); err != nil {
		logFollowError(ctx, key, err)
	}
}

func logFollowError(ctx context.Context, key string, err error) {
	if ctx.Err() == nil {
		logger.Error("a container's logs could not be followed", zap.Error(err), zap.String("container", key))
	}
}

// scanLines calls line for each line of r, without its line ending, until r ends. A line
// longer than the 1 MiB buffer keeps its first MiB (Truncate cuts it further) and the rest
// is skipped: unlike bufio.Scanner it never stops reading, so the stream never stalls.
func scanLines(r io.Reader, line func([]byte)) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		b, err := br.ReadSlice('\n')
		if len(b) > 0 {
			line(bytes.TrimRight(b, "\r\n"))
		}
		for errors.Is(err, bufio.ErrBufferFull) {
			_, err = br.ReadSlice('\n')
		}
		if err != nil {
			return
		}
	}
}

// readFrames splits Docker's multiplexed stream (an 8-byte header per frame: the stream in
// byte 0, the size big-endian in bytes 4-8) into lines. One goroutine in frame order, unlike
// stdcopy into two pipes, so stdout and stderr lines reach the file in the order Docker sent them.
func readFrames(r io.Reader, out, errs func([]byte)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	pending := map[byte][]byte{}
	feed := func(stream byte, chunk []byte) {
		line := lineOf(stream, out, errs)
		data := append(pending[stream], chunk...)
		for {
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				break
			}
			line(bytes.TrimRight(data[:i], "\r"))
			data = data[i+1:]
		}
		if len(data) > 1<<20 { // a line without end: its first MiB is kept, Truncate cuts it further
			line(data)
			data = nil
		}
		pending[stream] = bytes.Clone(data)
	}
	end := func(err error) error {
		for stream, rest := range pending {
			if len(rest) > 0 {
				lineOf(stream, out, errs)(rest)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	var hdr [8]byte
	chunk := make([]byte, 64<<10) // a frame is read in chunks: its size comes from the stream
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return end(err)
		}
		for size := int(binary.BigEndian.Uint32(hdr[4:])); size > 0; {
			n, err := io.ReadFull(br, chunk[:min(size, len(chunk))])
			feed(hdr[0], chunk[:n])
			if err != nil {
				return end(err)
			}
			size -= n
		}
	}
}

func lineOf(stream byte, out, errs func([]byte)) func([]byte) {
	if t := stdcopy.StdType(stream); t == stdcopy.Stdout || t == stdcopy.Stdin {
		return out
	}
	return errs
}
