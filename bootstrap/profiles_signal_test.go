//go:build !windows

package bootstrap

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/forgeplex/appkit"
)

const profileSignalHelperEnv = "APPKIT_BOOTSTRAP_PROFILE_SIGNAL_HELPER"
const profileSignalHelperModeEnv = "APPKIT_BOOTSTRAP_PROFILE_SIGNAL_MODE"

func TestRunnerStopsOnOSSignal(t *testing.T) {
	for _, signal := range []struct {
		name string
		sig  syscall.Signal
	}{
		{name: "SIGINT", sig: syscall.SIGINT},
		{name: "SIGTERM", sig: syscall.SIGTERM},
	} {
		t.Run(signal.name, func(t *testing.T) { runSignalHelper(t, signal.sig, "runner") })
	}
}

func TestCoreStartDoesNotHandleOSSignal(t *testing.T) {
	runSignalHelper(t, syscall.SIGTERM, "core")
}

func runSignalHelper(t *testing.T, signal syscall.Signal, mode string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCoreSignalHelper$")
	env := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, profileSignalHelperEnv+"=") && !strings.HasPrefix(value, profileSignalHelperModeEnv+"=") {
			env = append(env, value)
		}
	}
	cmd.Env = append(env,
		profileSignalHelperEnv+"=1",
		profileSignalHelperModeEnv+"="+mode,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	lines := make(chan string, 8)
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
		if !finished {
			_ = cmd.Process.Kill()
			<-wait
		}
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
					t.Fatalf("child %s failed: %s", mode, line)
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
	if mode == "core" {
		select {
		case err := <-wait:
			finished = true
			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("Core.Start child should exit from the default %s action; got %v", signal, err)
			}
			status, ok := exitErr.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != signal {
				t.Fatalf("Core.Start child status = %v, want terminated by %s", exitErr.Sys(), signal)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("Core.Start child did not exit after %s", signal)
		}
		for line := range lines {
			if line == "STOPPED" {
				t.Fatal("Core.Start unexpectedly handled the process signal")
			}
		}
		return
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

func TestCoreSignalHelper(t *testing.T) {
	if os.Getenv(profileSignalHelperEnv) != "1" {
		return
	}
	options := ProfileOptions{Service: "signalprofile"}
	options.Modules = func(ProfileDeps) ([]appkit.Module, error) {
		return []appkit.Module{profileTestModule{name: "signal-fixture", register: func(reg *appkit.Registry) error {
			reg.OnStart(appkit.StageInfra, func(context.Context) error {
				_, err := fmt.Fprintln(os.Stdout, "READY")
				return err
			})
			reg.OnStop(func(context.Context) error {
				_, err := fmt.Fprintln(os.Stdout, "STOPPED")
				return err
			})
			return nil
		}}}, nil
	}
	if os.Getenv(profileSignalHelperModeEnv) == "runner" {
		if err := NewRunner(options).Run(context.Background()); err != nil {
			_, _ = fmt.Fprintf(os.Stdout, "APP_ERROR: %v\n", err)
			t.Fatalf("Runner.Run: %v", err)
		}
		return
	}
	core, err := NewCore(context.Background(), options)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stdout, "APP_ERROR: %v\n", err)
		t.Fatalf("NewCore: %v", err)
	}
	running, err := core.Start(context.Background())
	if err != nil {
		_, _ = fmt.Fprintf(os.Stdout, "APP_ERROR: %v\n", err)
		t.Fatalf("Core.Start: %v", err)
	}
	if err := running.Wait(); err != nil {
		_, _ = fmt.Fprintf(os.Stdout, "APP_ERROR: %v\n", err)
		t.Fatalf("RunningProfile.Wait: %v", err)
	}
}
