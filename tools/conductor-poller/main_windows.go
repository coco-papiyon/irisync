package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"
)

const childFlag = "--conductor-child"

func main() {
	if len(os.Args) >= 3 && os.Args[1] == childFlag {
		os.Exit(runChild(os.Args[2:]))
	}
	if len(os.Args) == 4 && os.Args[1] == "--design-adapter" {
		if err := runDesignAdapter(os.Args[2], os.Args[3]); err != nil {
			slog.Error("Codex design failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "--implementation-adapter" {
		if err := runImplementationStep(os.Args[2], os.Args[3]); err != nil {
			slog.Error("implementation step failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && (os.Args[1] == "--pr-review-adapter" || os.Args[1] == "--merge-check-adapter") {
		if err := runReviewAdapter(os.Args[1], os.Args[2]); err != nil {
			slog.Error("PR review step failed", "error", err)
			os.Exit(1)
		}
		return
	}
	configPath := flag.String("config", ".conductor/poller.example.yaml", "YAML configuration file")
	once := flag.Bool("once", false, "poll once and wait for eligible runs")
	dashboardOnly := flag.Bool("dashboard-only", false, "serve the dashboard without running issue workflows")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *configPath, *once, *dashboardOnly); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("poller stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, configPath string, once, dashboardOnly bool) error {
	if once && dashboardOnly {
		return fmt.Errorf("-once and -dashboard-only cannot be used together")
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	gh, err := newGitHubProject(ctx, cfg)
	if err != nil {
		return err
	}
	if err := gh.resolveProjectID(ctx); err != nil {
		return err
	}
	cfg = gh.cfg
	if dashboardOnly {
		return runDashboardOnly(ctx, cfg, gh, configPath)
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return err
	}
	db, closeState, err := openState(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer closeState()
	if cfg.DashboardAddr != "" {
		stop, err := startDashboard(ctx, cfg.DashboardAddr, gh, db.db, false, configPath)
		if err != nil {
			return err
		}
		defer stop()
	}
	conductor, err := newConductor(cfg)
	if err != nil {
		return err
	}
	return (&poller{cfg: cfg, github: gh, conductor: conductor, db: db}).loop(ctx, once)
}

type poller struct {
	cfg           config
	github        githubProject
	conductor     conductorRunner
	db            *state
	workers       sync.WaitGroup
	slots         chan struct{}
	designMu      sync.Mutex
	activeDesigns map[string]bool
	nextItem      int
}

func (p *poller) loop(ctx context.Context, once bool) error {
	p.slots = make(chan struct{}, p.cfg.ParallelRuns)
	p.activeDesigns = make(map[string]bool)
	defer p.workers.Wait()

	delay := p.cfg.PollInterval
	for {
		err := p.poll(ctx)
		if once || ctx.Err() != nil {
			return err
		}
		if err == nil {
			delay = p.cfg.PollInterval
		} else {
			slog.Warn("poll failed; retrying", "error", err, "delay", delay)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if err != nil {
			delay = min(delay*2, p.cfg.PollInterval*4)
		}
	}
}

func (p *poller) poll(ctx context.Context) error {
	items, err := p.github.projectIssues(ctx)
	if err != nil {
		return err
	}
	promoted, promotionErr := p.github.promotePending(ctx, items)
	started, eligible := 0, 0
	start := p.nextItem
	for offset := range items {
		index := (start + offset) % len(items)
		item := items[index]
		if err := ctx.Err(); err != nil {
			return err
		}
		if !p.github.runnable(item) {
			continue
		}
		if p.cfg.AutoReady && item.Status != "Ready" {
			tracked, err := p.db.tracksIssue(ctx, item)
			if err != nil {
				return err
			}
			if !tracked {
				continue
			}
		}
		eligible++
		if item.Status == "Design Review" && !p.cfg.implementationEnabled(item.Number) {
			p.designMu.Lock()
			active := p.activeDesigns[item.ID]
			p.designMu.Unlock()
			if !active {
				if err := p.advanceDesign(ctx, item); err != nil {
					slog.Warn("design approval could not be checked", "issue", item.Number, "error", err)
				}
			}
			continue
		}
		select {
		case p.slots <- struct{}{}:
		default:
			continue
		}
		if p.cfg.designEnabled(item.Number) {
			if !p.launchDesign(ctx, item) {
				<-p.slots
				continue
			}
		} else {
			runID, err := p.db.claim(ctx, item, p.cfg.MaxAttempts)
			if err != nil || runID == 0 {
				<-p.slots
				if err != nil {
					return err
				}
				continue
			}
			p.workers.Add(1)
			go p.execute(ctx, item, runID)
		}
		started++
		// Rotate the starting point so a review that needs repeated polling
		// cannot take the only worker slot every cycle.
		if p.cfg.AutoReady {
			p.nextItem = (index + 1) % len(items)
		}
	}
	slog.Info("project polled", "ready", eligible, "started", started, "promoted", promoted)
	return promotionErr
}

func (p *poller) execute(ctx context.Context, item projectIssue, runID int64) {
	defer p.workers.Done()
	defer func() { <-p.slots }()
	logPath := filepath.Join(p.cfg.DataDir, fmt.Sprintf("run-%d.log", runID))
	runCtx, cancel := context.WithTimeout(ctx, p.cfg.WorkflowTimeout)
	defer cancel()
	result := p.conductor.run(runCtx, logPath)
	saveCtx, stopSave := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopSave()
	if err := p.db.finish(saveCtx, runID, logPath, result); err != nil {
		slog.Error("could not save run result", "run", runID, "error", err)
	}
	slog.Info("workflow finished", "issue", item.Number, "run", runID, "outcome", result.outcome, "exit_code", result.exitCode)
}
