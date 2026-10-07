package agentdesk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"tgcontrol/internal/procutil"
)

// This helper is a real child process; it exercises pipes, cancellation,
// restart and response framing without a GPU model or production files.
func TestBridgeProcess(t *testing.T) {
	if os.Getenv("AGENTDESK_FAKE_BRIDGE") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     string
			Method string
		}
		_ = json.Unmarshal(scanner.Bytes(), &request)
		if request.Method == "slow" {
			time.Sleep(10 * time.Second)
		}
		if request.Method == "grandchild" {
			child := procutil.Hidden(exec.Command(os.Args[0], "-test.run=TestBridgeGrandchild"))
			child.Env = append(os.Environ(), "AGENTDESK_TREE_CHILD=1")
			// Inherit the worker's process group, just as bundled FFmpeg does.
			child.Stdout = os.Stdout
			if err := child.Start(); err != nil {
				os.Exit(3)
			}
			fmt.Printf("{\"protocol\":1,\"id\":%q,\"ok\":true,\"result\":{\"pid\":%d}}\n", request.ID, child.Process.Pid)
			continue
		}
		if request.Method == "crash" {
			os.Exit(2)
		}
		if request.Method == "bad_id" {
			request.ID = "wrong"
		}
		if request.Method == "remote_error" {
			fmt.Printf("{\"protocol\":1,\"id\":%q,\"ok\":false,\"error\":{\"code\":\"path_denied\",\"message\":\"denied\"}}\n", request.ID)
			continue
		}
		fmt.Printf("{\"protocol\":1,\"id\":%q,\"ok\":true,\"result\":{\"pid\":%d,\"text\":\"Привет\"}}\n", request.ID, os.Getpid())
	}
	os.Exit(0)
}

func TestBridgeGrandchild(t *testing.T) {
	if os.Getenv("AGENTDESK_TREE_CHILD") != "1" {
		return
	}
	marker := os.Getenv("AGENTDESK_TREE_MARKER")
	for i := 0; i < 600; i++ {
		if err := os.WriteFile(marker, []byte(fmt.Sprint(time.Now().UnixNano())), 0o600); err != nil {
			os.Exit(0)
		}
		time.Sleep(50 * time.Millisecond)
	}
	os.Exit(0)
}

func TestCloseKillsWorkerDescendants(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "heartbeat")
	t.Setenv("AGENTDESK_TREE_MARKER", marker)
	client := fakeClient(t)
	var child struct{ PID int }
	if err := client.Call(context.Background(), "grandchild", nil, &child); err != nil {
		t.Fatal(err)
	}
	// On a regression, clean up only the child this test created, after proving
	// it is still writing this test's unique heartbeat file.
	t.Cleanup(func() {
		first, _ := os.ReadFile(marker)
		time.Sleep(70 * time.Millisecond)
		second, _ := os.ReadFile(marker)
		if string(first) != string(second) {
			if owned, err := os.FindProcess(child.PID); err == nil {
				_ = owned.Kill()
			}
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for !regularMarker(marker) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !regularMarker(marker) {
		t.Fatal("test child did not start")
	}
	client.Close()
	first, _ := os.ReadFile(marker)
	time.Sleep(150 * time.Millisecond)
	second, _ := os.ReadFile(marker)
	if string(first) != string(second) {
		t.Fatal("worker descendant survived Close")
	}
}

func regularMarker(path string) bool {
	data, err := os.ReadFile(path)
	return err == nil && len(data) > 0
}

func fakeClient(t *testing.T) *Client {
	t.Helper()
	client, err := New(Config{Executable: os.Args[0], Args: []string{"-test.run=TestBridgeProcess", "--"},
		DataDir: t.TempDir(), AllowedRoots: []string{t.TempDir()}, Env: []string{"AGENTDESK_FAKE_BRIDGE=1"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestWarmWorkerAndRemoteErrors(t *testing.T) {
	client := fakeClient(t)
	var first, second struct {
		PID  int
		Text string
	}
	if err := client.Call(context.Background(), "hello", nil, &first); err != nil {
		t.Fatal(err)
	}
	var remote *RemoteError
	if err := client.Call(context.Background(), "remote_error", nil, nil); !errors.As(err, &remote) || remote.Code != "path_denied" {
		t.Fatalf("remote error: %v", err)
	}
	if err := client.Call(context.Background(), "hello", nil, &second); err != nil {
		t.Fatal(err)
	}
	if first.PID != second.PID || first.Text != "Привет" {
		t.Fatalf("worker not reused: %v %v", first, second)
	}
}

func TestCancellationAndCrashRestartWithoutReplay(t *testing.T) {
	client := fakeClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := client.Call(ctx, "slow", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel: %v", err)
	}
	if err := client.Call(context.Background(), "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(context.Background(), "crash", nil, nil); err == nil {
		t.Fatal("crash passed")
	}
	if err := client.Call(context.Background(), "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(context.Background(), "bad_id", nil, nil); err == nil {
		t.Fatal("wrong ID passed")
	}
	if err := client.Call(context.Background(), "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCloseInterruptsActiveRequest(t *testing.T) {
	client := fakeClient(t)
	finished := make(chan error, 1)
	go func() { finished <- client.Call(context.Background(), "slow", nil, nil) }()
	time.Sleep(100 * time.Millisecond)
	client.Close()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("active call passed after close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not interrupt request")
	}
	if err := client.Call(context.Background(), "hello", nil, nil); err == nil {
		t.Fatal("closed client accepted request")
	}
}

func TestRealPackagedBridge(t *testing.T) {
	exe := os.Getenv("AGENTDESK_TEST_EXE")
	if exe == "" {
		t.Skip("set AGENTDESK_TEST_EXE to test the packaged bridge")
	}
	folder := t.TempDir()
	client, err := New(Config{Executable: exe, DataDir: filepath.Join(folder, "data"), AllowedRoots: []string{folder}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var caps struct{ Protocol int }
	if err := client.Call(context.Background(), "system.capabilities", nil, &caps); err != nil || caps.Protocol != 1 {
		t.Fatalf("caps: %v %+v", err, caps)
	}
	var bundle Bundle
	if err := client.Call(context.Background(), "task.create", map[string]any{"text": "Голос с телефона"}, &bundle); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(folder, "Лог с пробелами.txt")
	if err := os.WriteFile(path, []byte("тест"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(context.Background(), "task.attach", map[string]any{"task_id": bundle.TaskID, "path": path}, &bundle); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	shot, err := client.Capture(context.Background(), "region", bundle.TaskID, []int{0, 0, 160, 80})
	if err != nil || shot.Width != 160 || shot.Height != 80 {
		t.Fatalf("capture: %v %+v", err, shot)
	}
	if err := client.Call(context.Background(), "task.bundle", map[string]any{"task_id": bundle.TaskID}, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Paths) != 2 || bundle.Text != "Голос с телефона" {
		t.Fatalf("bundle: %+v", bundle)
	}
	for _, path := range bundle.Paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
}
