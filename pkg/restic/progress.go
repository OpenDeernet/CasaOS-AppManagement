package restic

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/backupprogress"
)

// How far a backup has got, from what `restic backup --json` prints while it runs.

// streamer runs restic and hands each line it prints to line as it comes.
type streamer func(ctx context.Context, env []string, line func([]byte), args ...string) error

// maxLine is the longest line read; a longer one (a status naming deep paths) is skipped, not fatal.
const maxLine = 1 << 20

// stopGrace is how long restic has to let go of its lock once asked to stop, before it is killed.
const stopGrace = 10 * time.Second

func execStreamer(ctx context.Context, env []string, line func([]byte), args ...string) error {
	command := exec.CommandContext(ctx, "restic", args...)
	command.Env = append(os.Environ(), env...)
	// asked to stop, restic removes its own lock; killed outright, it leaves one behind
	command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
	command.WaitDelay = stopGrace

	var stderr strings.Builder
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}

	if err := command.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return ErrNotInstalled
		}

		return err
	}

	reader := bufio.NewReaderSize(stdout, 64*1024)
	for {
		text, readErr := readLine(reader)
		if text != nil {
			line(text)
		}
		if readErr != nil {
			break
		}
	}

	if err := command.Wait(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return &ExitError{Code: exit.ExitCode(), Message: lastLines(resticMessage(stderr.String()))}
		}

		return err
	}

	return nil
}

// readLine reads one line; a line longer than maxLine is read to its end and dropped (nil).
func readLine(reader *bufio.Reader) ([]byte, error) {
	var text []byte
	for {
		chunk, isPrefix, err := reader.ReadLine()
		if len(text)+len(chunk) <= maxLine {
			text = append(text, chunk...)
		} else {
			text = nil
		}
		if err != nil || !isPrefix {
			if len(text) == 0 {
				return nil, err
			}

			return text, err
		}
		if text == nil {
			// over the limit: keep reading to the end of the line, keep nothing
			for isPrefix && err == nil {
				_, isPrefix, err = reader.ReadLine()
			}

			return nil, err
		}
	}
}

// resticMessage reads what restic said on stderr: with --json its errors are JSON objects, and
// what the history keeps is their message, not the object.
func resticMessage(stderr string) string {
	lines := []string{}
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var said struct {
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		switch {
		case json.Unmarshal([]byte(line), &said) != nil:
			lines = append(lines, line)
		case said.Message != "":
			lines = append(lines, said.Message)
		case said.Error.Message != "":
			lines = append(lines, said.Error.Message)
		default:
			lines = append(lines, line)
		}
	}

	return strings.Join(lines, "\n")
}

// lastLines is the end of what restic said, the lines that say why.
func lastLines(message string) string {
	message = strings.TrimSpace(message)
	if lines := strings.Split(message, "\n"); len(lines) > 6 {
		message = strings.Join(lines[len(lines)-6:], "\n")
	}

	return message
}

// parseStatus reads one `status` message. The totals are restic's count so far until its scan
// is over, which `seconds_remaining` appearing says; before that they are left unknown.
func parseStatus(line []byte) (backupprogress.Stats, bool) {
	var status struct {
		MessageType      string   `json:"message_type"`
		SecondsElapsed   float64  `json:"seconds_elapsed"`
		SecondsRemaining *float64 `json:"seconds_remaining"`
		TotalFiles       int64    `json:"total_files"`
		FilesDone        int64    `json:"files_done"`
		TotalBytes       int64    `json:"total_bytes"`
		BytesDone        int64    `json:"bytes_done"`
		CurrentFiles     []string `json:"current_files"`
	}
	if err := json.Unmarshal(line, &status); err != nil || status.MessageType != "status" {
		return backupprogress.Stats{}, false
	}

	stats := backupprogress.Stats{Bytes: status.BytesDone, Files: status.FilesDone}
	if status.SecondsElapsed > 0 {
		stats.Speed = int64(float64(status.BytesDone) / status.SecondsElapsed)
	}
	if status.SecondsRemaining != nil {
		stats.TotalBytes, stats.TotalFiles = status.TotalBytes, status.TotalFiles
		eta := int64(*status.SecondsRemaining)
		stats.ETA = &eta
	}
	if len(status.CurrentFiles) > 0 {
		stats.Current = status.CurrentFiles[0]
	}

	return stats, true
}
