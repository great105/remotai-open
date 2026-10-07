// Package agentdesk connects a trusted local Go backend to AgentDeskBridge.
// It opens no network listener. The embedding server owns authentication.
package agentdesk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"time"

	"tgcontrol/internal/procutil"
)

type Config struct {
	Executable string
	// Args precede --stdio (source mode: []string{"bridge_main.py"}).
	Args           []string
	DataDir        string
	AllowedRoots   []string
	Env            []string
	Stderr         io.Writer
	RequestTimeout time.Duration
	// TGControl: procutil.Hidden(exec.CommandContext(ctx, name, args...)).
	Command func(context.Context, string, ...string) *exec.Cmd
}

type RemoteError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RemoteError) Error() string { return e.Code + ": " + e.Message }

type response struct {
	Protocol int             `json:"protocol"`
	ID       string          `json:"id"`
	OK       bool            `json:"ok"`
	Result   json.RawMessage `json:"result"`
	Error    *RemoteError    `json:"error"`
}

type Client struct {
	config Config
	gate   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	cmd    *exec.Cmd
	input  io.WriteCloser
	reader *bufio.Reader
	done   chan struct{}
	seq    uint64
}

func New(config Config) (*Client, error) {
	if config.Executable == "" || config.DataDir == "" || len(config.AllowedRoots) == 0 {
		return nil, errors.New("executable, data directory and allowed file roots are required")
	}
	for _, root := range config.AllowedRoots {
		if root == "" {
			return nil, errors.New("allowed file root is empty")
		}
	}
	config.Args = append([]string(nil), config.Args...)
	config.AllowedRoots = append([]string(nil), config.AllowedRoots...)
	config.Env = append([]string(nil), config.Env...)
	ctx, cancel := context.WithCancel(context.Background())
	client := &Client{config: config, gate: make(chan struct{}, 1), ctx: ctx, cancel: cancel}
	client.gate <- struct{}{}
	return client, nil
}

func (c *Client) start() error {
	if c.cmd != nil {
		select {
		case <-c.done:
			c.stop()
		default:
			return nil
		}
	}
	args := append([]string(nil), c.config.Args...)
	args = append(args, "--stdio", "--data-dir", c.config.DataDir)
	for _, root := range c.config.AllowedRoots {
		args = append(args, "--allow-root", root)
	}
	command := c.config.Command
	if command == nil {
		command = hiddenCommand
	}
	cmd := command(c.ctx, c.config.Executable, args...)
	procutil.Prepare(cmd)
	cmd.Env = append(cmd.Environ(), c.config.Env...)
	cmd.Stderr = c.config.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		input.Close()
		output.Close()
		return err
	}
	c.cmd, c.input, c.reader = cmd, input, bufio.NewReader(output)
	c.done = make(chan struct{})
	done := c.done
	go func() { _ = cmd.Wait(); close(done) }()
	return nil
}

func (c *Client) stop() {
	if c.cmd == nil {
		return
	}
	select {
	case <-c.done: // Do not address a PID belonging to a finished process.
	default:
		_ = procutil.KillTree(c.cmd)
	}
	c.input.Close()
	<-c.done
	c.cmd, c.input, c.reader, c.done = nil, nil, nil, nil
}

// Call keeps Whisper loaded and serializes task updates. Cancellation kills
// the worker; a later call restarts it. Interrupted operations are not replayed.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	timeout := c.config.RequestTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return errors.New("agentdesk client is closed")
	case <-c.gate:
	}
	defer func() { c.gate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.ctx.Err(); err != nil {
		return errors.New("agentdesk client is closed")
	}
	if params == nil {
		params = map[string]any{}
	}
	c.seq++
	id := strconv.FormatUint(c.seq, 10)
	request := struct {
		Protocol int    `json:"protocol"`
		ID       string `json:"id"`
		Method   string `json:"method"`
		Params   any    `json:"params"`
	}{1, id, method, params}
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(payload) > 1<<20 {
		return errors.New("agentdesk request exceeds 1 MiB")
	}
	if err := c.start(); err != nil {
		return fmt.Errorf("start AgentDeskBridge: %w", err)
	}
	type exchange struct {
		line []byte
		err  error
	}
	input, reader := c.input, c.reader
	finished := make(chan exchange, 1)
	go func() {
		if _, err := input.Write(append(payload, '\n')); err != nil {
			finished <- exchange{err: err}
			return
		}
		line, err := reader.ReadBytes('\n')
		finished <- exchange{line: line, err: err}
	}()
	var outcome exchange
	select {
	case <-ctx.Done():
		c.stop()
		<-finished
		return ctx.Err()
	case <-c.ctx.Done():
		c.stop()
		<-finished
		return errors.New("agentdesk client is closed")
	case outcome = <-finished:
	}
	if outcome.err != nil {
		c.stop()
		return fmt.Errorf("AgentDeskBridge connection lost: %w", outcome.err)
	}
	var reply response
	if err := json.Unmarshal(outcome.line, &reply); err != nil {
		c.stop()
		return fmt.Errorf("invalid AgentDeskBridge response: %w", err)
	}
	if reply.Protocol != 1 || reply.ID != id {
		c.stop()
		return errors.New("AgentDeskBridge protocol or request ID mismatch")
	}
	if !reply.OK {
		if reply.Error == nil {
			return errors.New("AgentDeskBridge failed without error details")
		}
		return reply.Error
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(reply.Result, result)
}

// Close interrupts any active request and releases GPU memory.
func (c *Client) Close() error {
	c.cancel()
	<-c.gate
	defer func() { c.gate <- struct{}{} }()
	c.stop()
	return nil
}

type Bundle struct {
	TaskID string   `json:"task_id"`
	Text   string   `json:"text"`
	Prompt string   `json:"prompt"`
	Paths  []string `json:"paths"`
}

type Transcript struct {
	TaskID   string  `json:"task_id"`
	Text     string  `json:"text"`
	Duration float64 `json:"duration"`
	Segments int     `json:"segments"`
	Device   string  `json:"device"`
	Bundle   Bundle  `json:"bundle"`
}

type Screenshot struct {
	TaskID string `json:"task_id"`
	Path   string `json:"path"`
	Name   string `json:"name"`
	Dir    string `json:"dir"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

func (c *Client) Transcribe(ctx context.Context, path, taskID string) (Transcript, error) {
	params := map[string]any{"path": path}
	if taskID != "" {
		params["task_id"] = taskID
	}
	var result Transcript
	err := c.Call(ctx, "audio.transcribe", params, &result)
	return result, err
}

func (c *Client) Capture(ctx context.Context, mode, taskID string, bounds []int) (Screenshot, error) {
	params := map[string]any{"mode": mode}
	if taskID != "" {
		params["task_id"] = taskID
	}
	if bounds != nil {
		params["bounds"] = bounds
	}
	var result Screenshot
	err := c.Call(ctx, "screen.capture", params, &result)
	return result, err
}
