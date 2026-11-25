package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type conductorRunner struct {
	executable string
	workDir    string
	workflow   string
	script     string
}

type runResult struct {
	outcome  string
	exitCode int
	err      error
}

func newConductor(cfg config) (conductorRunner, error) {
	executable, err := conductorExecutable(cfg.WorkDir)
	if err != nil {
		return conductorRunner{}, err
	}
	runner := conductorRunner{
		executable: executable,
		workDir:    cfg.WorkDir,
		workflow:   filepath.Join(cfg.WorkDir, ".conductor", "workflows", "smoke.yaml"),
		script:     filepath.Join(cfg.WorkDir, ".conductor", "scripts", "smoke.ps1"),
	}
	for _, path := range []string{runner.workflow, runner.script} {
		if _, err := os.Stat(path); err != nil {
			return conductorRunner{}, err
		}
	}
	return runner, nil
}

func conductorExecutable(workDir string) (string, error) {
	local := filepath.Join(workDir, ".conductor", ".local", "conductor-runtime", "Scripts", "conductor.exe")
	info, err := os.Stat(local)
	if errors.Is(err, os.ErrNotExist) {
		return exec.LookPath("conductor")
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("local Conductor executable is not a regular file: %s", local)
	}
	return filepath.Abs(local)
}

func (r conductorRunner) run(ctx context.Context, logPath string) runResult {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return classifyRun(ctx, err)
	}
	defer log.Close()
	args := []string{r.executable, "--silent", "run", r.workflow, "--no-interactive",
		"--input", "mode=success", "--input", "work_dir=" + r.workDir, "--input", "script_path=" + r.script}
	return classifyRun(ctx, runInJob(ctx, r.workDir, log, args...))
}

func classifyRun(ctx context.Context, err error) runResult {
	if errors.Is(err, errCodexLimited) {
		return runResult{"codex_limited", -1, err}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return runResult{"timed_out", -1, err}
	}
	if errors.Is(err, context.Canceled) {
		return runResult{"interrupted", -1, err}
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return runResult{"failed", exit.ExitCode(), err}
		}
		return runResult{"failed", -1, err}
	}
	if ctx.Err() != nil {
		return classifyRun(ctx, ctx.Err())
	}
	return runResult{"succeeded", 0, nil}
}

func runInJob(ctx context.Context, dir string, log *os.File, args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	defer func() {
		if job != 0 {
			windows.CloseHandle(job)
		}
	}()
	jobInfo := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	jobInfo.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&jobInfo)), uint32(unsafe.Sizeof(jobInfo))); err != nil {
		return err
	}

	// The helper waits for one byte before starting Conductor. Its descendants
	// therefore inherit the job even if Conductor launches children immediately.
	cmd := exec.Command(exe, append([]string{childFlag}, args...)...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	cmd.Env = append(cmd.Environ(), "CONDUCTOR_NO_UPDATE_CHECK=1", "PYTHONUTF8=1",
		"CONDUCTOR_HOME="+filepath.Join(filepath.Dir(log.Name()), "conductor"))
	start, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer start.Close()
	if err := cmd.Start(); err != nil {
		return err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, process)
		windows.CloseHandle(process)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		_, err = start.Write([]byte{1})
	}
	start.Close()
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("start process in Windows Job Object: %w", err)
	}
	result := make(chan error, 1)
	go func() { result <- cmd.Wait() }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		if err := windows.TerminateJobObject(job, 1); err != nil {
			windows.CloseHandle(job)
			job = 0
		}
		<-result
		return ctx.Err()
	}
}

func runChild(args []string) int {
	var start [1]byte
	if _, err := io.ReadFull(os.Stdin, start[:]); err != nil || start[0] != 1 {
		return 1
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
