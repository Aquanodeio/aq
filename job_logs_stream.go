package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
)

// `aq job logs -f` tries the run-log SSE stream before falling back to the
// existing 2s poll loop in runJobLogs (job_logs.go:65-126). The stream never
// fails silently: any transport error, non-200, or a terminal/end frame all
// hand off to a poll starting from the offset the stream last saw, so a user
// never sees the tail just stop.
//
// runJobLogs itself is left untouched on purpose. It always starts at offset
// 0, and reusing it for the handoff would reprint everything the stream
// already showed. pollRunLogsFrom below is deliberately the same shape as
// that loop, just parameterized by a starting offset, rather than adding a
// startOffset field to job_logs.go's poll loop.

// maxSSELineSize raises the scanner's token limit well past bufio.Scanner's
// 64KB default: a `chunk` frame's data line can carry on the order of a
// megabyte of log text.
const maxSSELineSize = 8 * 1024 * 1024

// sseChunkFrame mirrors the stream's `event: chunk` data. Field names match
// api.RunLogChunk's poll fields deliberately, so a client can drop from
// stream to poll at the last nextOffset it saw.
type sseChunkFrame struct {
	Chunk      string `json:"chunk"`
	NextOffset int64  `json:"nextOffset"`
	Size       int64  `json:"size"`
	Truncated  bool   `json:"truncated"`
}

// sseEndFrame mirrors `event: end`. reason is one of exited|rotated|gone;
// rotated means bytes were dropped server-side and NextOffset is already 0.
type sseEndFrame struct {
	Reason     string `json:"reason"`
	NextOffset int64  `json:"nextOffset"`
}

// runJobLogsFollow is the entry point `jobLogs` calls. A plain (non-follow)
// read never touches the stream: one poll is already the right amount of
// work, and runJobLogs does that unmodified.
func runJobLogsFollow(opts jobLogsOptions) error {
	if !opts.follow {
		return runJobLogs(opts)
	}

	out := opts.out
	if out == nil {
		out = os.Stdout
	}
	errOut := opts.errOut
	if errOut == nil {
		errOut = os.Stderr
	}
	newClient := opts.client
	if newClient == nil {
		newClient = newControlClient
	}
	client := newClient(opts.cred)

	jobID, err := resolveJobID(client, opts.jobRef)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	offset, warnedUnreachable, streamErr := streamRunLogs(ctx, client, jobID, opts, out, errOut)
	if ctx.Err() != nil {
		// Ctrl-C: the user asked for this, not a stream failure. The request
		// context cancellation already closed the connection; there is
		// nothing left to fall back to.
		return nil
	}
	if streamErr != nil {
		fmt.Fprintf(errOut, "aq: log stream: %v, falling back to polling\n", streamErr)
	}

	return pollRunLogsFrom(client, jobID, opts, out, errOut, offset, warnedUnreachable)
}

// streamRunLogs opens the SSE stream and prints chunks as they arrive. It
// returns the offset to resume polling from and whether the "can't reach the
// machine" warning already fired (so the poll fallback doesn't repeat it).
//
// err is non-nil only for a genuine failure (transport error, non-200,
// malformed frame, or the body closing without an end/terminal frame): a
// clean terminal/end frame returns a nil err, since handing off to the poll
// from there is the ordinary path the contract describes, not a failure.
func streamRunLogs(ctx context.Context, client *api.Client, jobID string, opts jobLogsOptions, out, errOut io.Writer) (offset int64, warnedUnreachable bool, err error) {
	req, err := client.NewRunLogsStreamRequest(ctx, jobID, opts.runID, offset, opts.attempt)
	if err != nil {
		return 0, false, err
	}

	// This request must never inherit the JSON client's request timeout:
	// that timeout bounds the whole request including reading the body, and
	// a quiet tail with no chunk for that long would get killed exactly when
	// nothing is wrong. Only ctx (Ctrl-C) or the server closing the
	// connection should end this read.
	streamClient := &http.Client{Transport: client.HTTP.Transport}

	resp, err := streamClient.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return 0, false, fmt.Errorf("unexpected response (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineSize)

	var event string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			event = ""
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			switch event {
			case "chunk":
				var f sseChunkFrame
				if jsonErr := json.Unmarshal([]byte(data), &f); jsonErr != nil {
					return offset, warnedUnreachable, fmt.Errorf("malformed chunk frame: %w", jsonErr)
				}
				if f.Chunk != "" {
					fmt.Fprint(out, f.Chunk)
				}
				// Always the server's nextOffset, never offset+len(chunk): a
				// capped read would otherwise make the follower skip bytes
				// it never saw.
				offset = f.NextOffset
				if f.Truncated {
					fmt.Fprintln(errOut, "aq: this log got long enough that its oldest output was dropped; you are seeing the retained tail")
				}
			case "heartbeat":
				// Nothing to print, this frame exists only to prove the
				// connection is still alive.
			case "end":
				var f sseEndFrame
				if jsonErr := json.Unmarshal([]byte(data), &f); jsonErr != nil {
					return offset, warnedUnreachable, fmt.Errorf("malformed end frame: %w", jsonErr)
				}
				if f.Reason == "rotated" {
					fmt.Fprintln(errOut, "aq: the log file rotated while streaming; some output may not have been shown, resuming from the start of the new file")
				}
				return f.NextOffset, warnedUnreachable, nil
			case "terminal":
				chunk, parseErr := parseTerminalFrame([]byte(data))
				if parseErr != nil {
					return offset, warnedUnreachable, fmt.Errorf("malformed terminal frame: %w", parseErr)
				}
				if chunk.Source == "unreachable" {
					fmt.Fprintln(errOut, "aq: can't reach the machine to read its log right now, this says nothing about whether your run is still going")
					warnedUnreachable = true
				}
				return chunk.NextOffset, warnedUnreachable, nil
			}
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return offset, warnedUnreachable, scanErr
	}
	// The body closed without an end/terminal frame: the server hung up
	// early. Falling back is the safe read of that, not a hang.
	return offset, warnedUnreachable, io.ErrUnexpectedEOF
}

// parseTerminalFrame decodes an `event: terminal` frame's data, which the
// contract defines as "the exact JSON body the poll handler would have
// returned" for a non-live source. Try it as the poll's plain data shape
// first (chunk/nextOffset/source/...); if that comes back empty, fall back to
// treating it as the orchestrator's {success,data,error} envelope and unwrap
// that. Accepting either shape means a change to whether the terminal frame
// is enveloped doesn't quietly turn into "the tail just stops."
func parseTerminalFrame(raw []byte) (*api.RunLogChunk, error) {
	var chunk api.RunLogChunk
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return nil, err
	}
	if chunk.NextOffset != 0 || chunk.Source != "" {
		return &chunk, nil
	}
	var env struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Data) > 0 {
		var inner api.RunLogChunk
		if err := json.Unmarshal(env.Data, &inner); err == nil {
			return &inner, nil
		}
	}
	return &chunk, nil
}

// pollRunLogsFrom is the fallback poll loop runJobLogsFollow hands off to
// once the stream ends (error, non-200, or a terminal/end frame). It mirrors
// runJobLogs's loop (job_logs.go:65-126), parameterized by a starting offset
// and the warnedUnreachable flag the stream may already have tripped:
// reusing runJobLogs directly here would reprint everything the stream
// already showed, and job_logs.go's own loop stays untouched so the plain
// (non-follow) path keeps behaving exactly as before.
//
// Only reached with opts.follow true (runJobLogsFollow's only caller of
// this), so it does not re-check opts.follow before looping.
func pollRunLogsFrom(client *api.Client, jobID string, opts jobLogsOptions, out, errOut io.Writer, offset int64, warnedUnreachable bool) error {
	sleep := opts.sleep
	if sleep == nil {
		sleep = time.Sleep
	}

	for polls := 0; ; polls++ {
		chunk, err := client.GetRunLogs(jobID, opts.runID, offset, opts.attempt)
		if err != nil {
			return fmt.Errorf("aq job logs: %w", err)
		}

		if chunk.Chunk != "" {
			fmt.Fprint(out, chunk.Chunk)
			offset = chunk.NextOffset
		}

		if chunk.Source == "unreachable" && !warnedUnreachable {
			fmt.Fprintln(errOut, "aq: can't reach the machine to read its log right now, this says nothing about whether your run is still going")
			warnedUnreachable = true
		}
		if chunk.Truncated {
			fmt.Fprintln(errOut, "aq: this log got long enough that its oldest output was dropped; you are seeing the retained tail")
		}

		if chunk.Source == "archived" || chunk.Source == "box_gone" {
			return nil
		}
		if opts.maxPolls > 0 && polls+1 >= opts.maxPolls {
			return nil
		}
		sleep(2 * time.Second)
	}
}
