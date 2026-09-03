package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunnerDeliversEventAndUsesDeterministicOrder(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "run", "success.d")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "events")
	writeHook(t, directory, "20-second", "#!/bin/sh\nprintf second: >> "+shellPath(output)+"\n/bin/cat >> "+shellPath(output)+"\n")
	writeHook(t, directory, "10-first", "#!/bin/sh\nprintf first: >> "+shellPath(output)+"\n/bin/cat >> "+shellPath(output)+"\n")
	event := validEvent()
	runner := testRunner(root)
	if err := runner.Run(event); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "first:") || !strings.HasPrefix(lines[1], "second:") {
		t.Fatalf("hook order/output = %q", data)
	}
	var decoded Event
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[0], "first:")), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != event {
		t.Fatalf("event = %#v, want %#v", decoded, event)
	}
}

func TestRunnerDoesNotExposeHookOutput(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "run", "failure.d")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeHook(t, directory, "notify", "#!/bin/sh\necho super-secret-response >&2\nexit 7\n")
	event := validEvent()
	event.Outcome = "failure"
	event.ExitCode = 1
	err := testRunner(root).Run(event)
	if err == nil || strings.Contains(err.Error(), "super-secret-response") || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunnerTimesOutProcessGroup(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "run", "success.d")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeHook(t, directory, "slow", "#!/bin/sh\n/bin/sleep 10\n")
	runner := testRunner(root)
	runner.Timeout = 100 * time.Millisecond
	started := time.Now()
	err := runner.Run(validEvent())
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestRunnerRejectsUnsafeEntries(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{name: "writable", setup: func(t *testing.T, path string) {
			writeHook(t, filepath.Dir(path), filepath.Base(path), "#!/bin/sh\nexit 0\n")
			if err := os.Chmod(path, 0o722); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink", setup: func(t *testing.T, path string) {
			if err := os.Symlink("/bin/true", path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "not executable", setup: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("not executable"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "run", "success.d")
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			test.setup(t, filepath.Join(directory, "unsafe"))
			if err := testRunner(root).Run(validEvent()); err == nil || !strings.Contains(err.Error(), "unsafe hook") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRunnerIgnoresMissingHookTree(t *testing.T) {
	runner := testRunner(filepath.Join(t.TempDir(), "missing"))
	if err := runner.Run(validEvent()); err != nil {
		t.Fatal(err)
	}
}

func validEvent() Event {
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	return Event{
		SchemaVersion: SchemaVersion, Action: "run", Outcome: "success", Environment: "production", Target: "database",
		BackupID: "20260903t120000z-test", StartedAt: started, FinishedAt: started.Add(time.Second), DurationMS: 1000,
	}
}

func testRunner(root string) Runner {
	return Runner{Root: root, Timeout: 30 * time.Second, RequiredUID: uint32(os.Getuid())}
}

func writeHook(t *testing.T, directory, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func shellPath(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}
