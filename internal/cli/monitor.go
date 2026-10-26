package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"crease/internal/monitor"
	"crease/internal/workspace"
)

func (a *app) monitor(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: crease monitor start|stop|status|run")
	}
	w, e := workspace.Find(a.cwd)
	if e != nil {
		return e
	}
	root := w.Root
	w.Close()
	switch args[0] {
	case "run":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return monitor.Run(ctx, root)
	case "status":
		r, e := monitor.Query(root, "status", "", 0)
		if e != nil {
			fmt.Fprintln(a.out, "Monitor is not running; commands use full scans.")
			return nil
		}
		fmt.Fprintf(a.out, "Monitor is running using %s, sequence %d.\n", r.Backend, r.Seq)
		return nil
	case "stop":
		_, e := monitor.Query(root, "stop", "", 0)
		if e != nil {
			return e
		}
		fmt.Fprintln(a.out, "Monitor stopped.")
		return nil
	case "start":
		if r, e := monitor.Query(root, "status", "", 0); e == nil {
			fmt.Fprintln(a.out, "Monitor is already running using", r.Backend)
			return nil
		}
		binary, e := os.Executable()
		if e != nil {
			return e
		}
		log, e := os.OpenFile(filepath.Join(root, ".crease", "monitor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if e != nil {
			return e
		}
		defer log.Close()
		cmd := exec.Command(binary, "monitor", "run")
		cmd.Dir = root
		cmd.Stdout = log
		cmd.Stderr = log
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if e = cmd.Start(); e != nil {
			return e
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		deadline := time.NewTimer(15 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case e := <-done:
				return fmt.Errorf("monitor could not start: %v; see .crease/monitor.log", e)
			case <-deadline.C:
				return errors.New("monitor startup timed out; see .crease/monitor.log")
			case <-tick.C:
				if r, e := monitor.Query(root, "status", "", 0); e == nil {
					fmt.Fprintln(a.out, "Monitor started using", r.Backend)
					return nil
				}
			}
		}
	default:
		return errors.New("usage: crease monitor start|stop|status|run")
	}
}
