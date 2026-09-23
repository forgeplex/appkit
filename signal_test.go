//go:build !windows

package appkit

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const signalHelperEnv = "APPKIT_RUN_SIGNAL_HELPER"

// TestRunStopsOnOSSignal characterizes the process signal contract in a child:
// App.Run must consume SIGINT/SIGTERM, run shutdown hooks, and return cleanly.
func TestRunStopsOnOSSignal(t *testing.T) {
	for _, signal := range []struct {
		name string
		sig  syscall.Signal
	}{
		{name: "SIGINT", sig: syscall.SIGINT},
		{name: "SIGTERM", sig: syscall.SIGTERM},
	} {
		t.Run(signal.name, func(t *testing.T) {
			runSignalHelper(t, signal.sig)
		})
	}
}

func runSignalHelper(t *testing.T, signal syscall.Signal) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunOSSignalHelper$")
	env := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, signalHelperEnv+"=") {
			env = append(env, value)
		}
	}
	cmd.Env = append(env, signalHelperEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	lines := make(chan string, 4)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	finished := false
	t.Cleanup(func() {
		if finished {
			return
		}
		_ = cmd.Process.Kill()
		<-wait
	})

	readMarker := func(want string) {
		t.Helper()
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("child exited before %q", want)
				}
				if line == want {
					return
				}
				if strings.HasPrefix(line, "APP_ERROR:") {
					t.Fatalf("child App.Run failed: %s", line)
				}
			case <-timer.C:
				t.Fatalf("timeout waiting for child marker %q", want)
			}
		}
	}

	readMarker("READY")
	if err := cmd.Process.Signal(signal); err != nil {
		t.Fatalf("send %s: %v", signal, err)
	}
	readMarker("STOPPED")
	select {
	case err := <-wait:
		finished = true
		if err != nil {
			t.Fatalf("child should exit cleanly after %s: %v", signal, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("child did not exit after %s", signal)
	}
}

// TestRunOSSignalHelper is entered only by TestRunStopsOnOSSignal's child process.
func TestRunOSSignalHelper(t *testing.T) {
	if os.Getenv(signalHelperEnv) != "1" {
		return
	}
	module := ModuleFunc("signal-fixture", func(reg *Registry) error {
		reg.OnStart(StageServer+1, func(context.Context) error {
			_, err := fmt.Fprintln(os.Stdout, "READY")
			return err
		})
		reg.OnStop(func(context.Context) error {
			_, err := fmt.Fprintln(os.Stdout, "STOPPED")
			return err
		})
		return nil
	})
	app := New([]Module{module},
		Security(SecurityUserFacing),
		HTTPAddr("127.0.0.1:0"),
		Logger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err := app.Run(context.Background()); err != nil {
		_, _ = fmt.Fprintf(os.Stdout, "APP_ERROR: %v\n", err)
		t.Fatalf("App.Run: %v", err)
	}
}
