package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStartAndCloseWritesRunMetadata(t *testing.T) {
	root := t.TempDir()
	session, err := Start(root)
	if err != nil {
		t.Fatal(err)
	}
	if session.PreviousRunUnclean() {
		t.Fatal("first run was reported as unclean")
	}
	state := readState(t, filepath.Join(root, "run-state.json"))
	if state.Clean {
		t.Fatal("active run must be marked unclean until Close")
	}
	if state.RunID != session.RunID() || state.GOOS != runtime.GOOS || state.GOARCH != runtime.GOARCH || state.PID <= 0 {
		t.Fatalf("run state metadata is incomplete: %+v", state)
	}

	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	state = readState(t, filepath.Join(root, "run-state.json"))
	if !state.Clean || state.EndedAt == "" {
		t.Fatalf("closed run state is not clean: %+v", state)
	}
	logData, err := os.ReadFile(session.AppLogPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), `"event":"run_start"`) || !strings.Contains(string(logData), `"event":"run_end"`) {
		t.Fatalf("run boundary records missing from app.log: %s", logData)
	}

	next, err := Start(root)
	if err != nil {
		t.Fatal(err)
	}
	if next.PreviousRunUnclean() {
		t.Fatal("clean previous run was reported as unclean")
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUncleanRunIsReportedOnNextStart(t *testing.T) {
	root := t.TempDir()
	first, err := Start(root)
	if err != nil {
		t.Fatal(err)
	}
	firstCrash := first.CrashPath()
	// Simulate a process terminated by SIGKILL: close descriptors without
	// writing run_end or changing run-state.json to clean.
	if _, err := deactivate(first); err != nil {
		t.Fatal(err)
	}
	if err := first.crash.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.logger.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Start(root)
	if err != nil {
		t.Fatal(err)
	}
	if !second.PreviousRunUnclean() {
		t.Fatal("unclean previous run was not reported")
	}
	if !PreviousRunUnclean() {
		t.Fatal("global previous-run status did not report the active unclean finding")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(firstCrash); err != nil {
		t.Fatalf("unclean run crash file should be retained: %v", err)
	}
}

func TestMalformedRunStateDoesNotDisableDiagnostics(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "run-state.json")
	if err := os.WriteFile(statePath, []byte(`{"truncated":`), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Start(root)
	if err != nil {
		t.Fatalf("Start() with malformed prior run state: %v", err)
	}
	if !session.PreviousRunUnclean() {
		t.Fatal("malformed prior run state was not treated as unclean")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStartRejectsSecondActiveSessionWithoutReplacingItsRunState(t *testing.T) {
	first, err := Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	secondRoot := t.TempDir()
	statePath := filepath.Join(secondRoot, "run-state.json")
	originalState := []byte(`{"schema_version":1,"run_id":"previous","started_at":"2026-01-01T00:00:00Z","clean":true}`)
	if err := os.WriteFile(statePath, originalState, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(secondRoot); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start() error = %v, want ErrAlreadyStarted", err)
	}
	gotState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotState) != string(originalState) {
		t.Fatalf("failed second Start changed prior run state:\n got %s\nwant %s", gotState, originalState)
	}
	if err := first.Logf("first session still active"); err != nil {
		t.Fatalf("first session was displaced: %v", err)
	}
}

func TestEmptyCrashFileRemovedOnCleanClose(t *testing.T) {
	session, err := Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	crashPath := session.CrashPath()
	info, err := os.Stat(crashPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("new crash file is not empty: %d", info.Size())
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(crashPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty crash file still exists, stat error: %v", err)
	}
}

func TestNonEmptyCrashFileRetainedOnCleanClose(t *testing.T) {
	session, err := Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	crashPath := session.CrashPath()
	if _, err := session.crash.WriteString("simulated panic output\n"); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(crashPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "simulated panic output\n" {
		t.Fatalf("crash output was changed: %q", data)
	}
}

func TestCrashOutputCapturesBackgroundGoroutinePanic(t *testing.T) {
	if root := os.Getenv("TGUP_DIAGNOSTICS_PANIC_HELPER"); root != "" {
		if _, err := Start(root); err != nil {
			panic(err)
		}
		go func() { panic("diagnostics helper panic") }()
		select {}
	}

	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashOutputCapturesBackgroundGoroutinePanic$")
	command.Env = append(os.Environ(), "TGUP_DIAGNOSTICS_PANIC_HELPER="+root)
	if err := command.Run(); err == nil {
		t.Fatal("panic helper process exited successfully")
	}
	if ctx.Err() != nil {
		t.Fatalf("panic helper process timed out: %v", ctx.Err())
	}

	crashFiles, err := filepath.Glob(filepath.Join(root, "logs", "crash-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(crashFiles) != 1 {
		t.Fatalf("crash files = %v, want exactly one", crashFiles)
	}
	data, err := os.ReadFile(crashFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "diagnostics helper panic") || !strings.Contains(string(data), "goroutine") {
		t.Fatalf("crash output does not contain panic and stack: %s", data)
	}
}

func TestLogRotationRetainsThreeBackups(t *testing.T) {
	session, err := Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("x", int(MaxLogBytes/2))
	for i := 0; i < 8; i++ {
		if err := session.Logf("rotation record %d %s", i, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= MaxLogBackups; index++ {
		path := session.AppLogPath() + "." + strconv.Itoa(index)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("backup %d missing: %v", index, err)
		}
	}
	if _, err := os.Stat(session.AppLogPath() + ".4"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected fourth backup, stat error: %v", err)
	}
	activeInfo, err := os.Stat(session.AppLogPath())
	if err != nil {
		t.Fatal(err)
	}
	if activeInfo.Size() > MaxLogBytes {
		t.Fatalf("active log exceeds configured limit: %d", activeInfo.Size())
	}
}

func TestLoggingRedactsCredentials(t *testing.T) {
	session, err := Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	apiHash := "abcdef0123456789"
	botToken := "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh"
	password := "very-secret-password"
	if err := Logf("api_hash=%s bot_token=%s", apiHash, botToken); err != nil {
		t.Fatal(err)
	}
	if err := LogError(errors.New("password: "+password), "upload failed"); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(session.AppLogPath())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, secret := range []string{apiHash, botToken, password} {
		if strings.Contains(text, secret) {
			t.Fatalf("credential leaked to app.log: %q in %s", secret, text)
		}
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("redaction marker missing from app.log: %s", text)
	}
}

func TestConcurrentLogging(t *testing.T) {
	session, err := Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	const records = 40
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for record := 0; record < records; record++ {
				if err := Logf("worker=%d record=%d", worker, record); err != nil {
					t.Errorf("concurrent Logf: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCloseAndLoggingIsSafe(t *testing.T) {
	session, err := Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for record := 0; record < 100; record++ {
				err := session.Logf("close-race worker=%d record=%d", worker, record)
				if err != nil && !errors.Is(err, ErrClosed) {
					t.Errorf("Logf() during Close = %v", err)
					return
				}
			}
		}(worker)
	}
	close(start)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if err := session.Close(); err != nil {
		t.Fatalf("repeated Close() = %v", err)
	}
}

func TestStartRejectsInvalidOrUnreadableRoot(t *testing.T) {
	base := t.TempDir()
	fileRoot := filepath.Join(base, "not-a-directory")
	if err := os.WriteFile(fileRoot, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(fileRoot); err == nil {
		t.Fatal("Start accepted a regular file as root")
	}

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permission checks are not reliable as root or on Windows")
	}
	readonlyRoot := filepath.Join(base, "readonly")
	if err := os.Mkdir(readonlyRoot, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(readonlyRoot, 0o700)
	if _, err := Start(readonlyRoot); err == nil {
		t.Fatal("Start accepted a read-only root")
	}
}

func readState(t *testing.T, path string) runState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state runState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}
