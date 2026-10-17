package dispatcher

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	board "pwnmesh/internal/board/model"
	"pwnmesh/internal/config"
	"pwnmesh/internal/modelconfig"
	"pwnmesh/internal/protocol"
)

type Runner interface {
	Run(context.Context, config.Worker, protocol.Job) (protocol.Result, error)
	Cleanup(context.Context, string, string) error
	Projects(context.Context) ([]string, error)
}
type checkpoint struct{ Facts, Hints, Open int }
type reasonWait struct {
	First, Changed time.Time
	Revision       int64
	Urgent         bool
}

const reasonQuietPeriod = 15 * time.Second
const reasonMaxWait = time.Minute

type task struct {
	Job          protocol.Job
	Worker       config.Worker
	Lease        Lease
	Cancel       context.CancelFunc
	Root         context.Context
	Execution    board.Execution
	LeaseTimeout time.Duration
	committedAt  atomic.Int64
	staleInputAt time.Time // Written before publishing this task to done.
	urgentInput  bool      // An invalid dependency is consumed only by success.
	// Owned by the run goroutine; same-run responses replace cumulative metrics.
	observedMetrics *protocol.DecisionMetrics
}
type finished struct {
	Task    *task
	Outcome string
	Err     error
}
type cleaned struct {
	ID, State string
	Err       error
}
type Scheduler struct {
	modelSettings     modelconfig.Settings
	modelLoaded       bool
	modelSettingsErr  error
	Config            config.Config
	Client            *Client
	Runner            Runner
	running           map[string]*task
	admitted          map[string]bool
	checkpoints       map[string]checkpoint
	reasonWaits       map[string]reasonWait
	curationWaits     map[string]reasonWait
	controlConflicts  map[string]time.Time
	unhealthy         map[string]time.Time
	incompatible      map[string]string
	rejected          map[string]time.Time
	deliveryWaits     map[string]time.Time
	cleanup           map[string]string
	cleaned           map[string]string
	done              chan finished
	cleanupDone       chan cleaned
	wakeup            chan struct{}
	nextWake          time.Time
	wg                sync.WaitGroup
	cursor            int
	cleanupCursor     int
	pendingExecutions []board.ExecutionSummary
	decisionRevisions map[string]int64
	schedules         map[string]board.SchedulePage
	generations       map[string]int64
	restartCleaned    map[string]int64
	leaseTimeout      time.Duration
	// CheckHealth overrides the model readiness probe in tests or embeddings.
	CheckHealth func(context.Context, config.Worker) error
}

func New(c config.Config, r Runner) *Scheduler {
	s := &Scheduler{Config: c, Runner: r, Client: &Client{Base: c.Server}, running: map[string]*task{}, admitted: map[string]bool{}, checkpoints: map[string]checkpoint{}, unhealthy: map[string]time.Time{}, rejected: map[string]time.Time{}, cleanup: map[string]string{}, cleaned: map[string]string{}, done: make(chan finished, c.Runtime.MaxWorkers), cleanupDone: make(chan cleaned, c.Runtime.MaxProjects+8), decisionRevisions: map[string]int64{}}
	s.configureGraphHandler()
	s.configureInputReader()
	s.schedules = map[string]board.SchedulePage{}
	s.generations = map[string]int64{}
	s.restartCleaned = map[string]int64{}
	s.reasonWaits = map[string]reasonWait{}
	s.controlConflicts = map[string]time.Time{}
	s.deliveryWaits = map[string]time.Time{}
	s.wakeup = make(chan struct{}, 1)
	return s
}

// Run holds dispatcher ownership through startup, scheduling and worker shutdown.
func (s *Scheduler) Run(ctx context.Context) error {
	parent := ctx
	ctx, release, err := s.acquireDispatcher(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := s.prepareRun(ctx); err != nil {
		return err
	}
	defer s.stopWorkers()
	return s.runLoop(parent, ctx)
}

// Step reconciles existing work before admitting new executions. Configuration
// errors must not prevent cancellation, recovery settlement or container cleanup.
func (s *Scheduler) Step(ctx context.Context) error {
	s.nextWake = time.Time{}
	if err := s.prepareStep(ctx); err != nil {
		return err
	}
	states, active, err := s.reconcileProjects(ctx)
	if err != nil {
		return err
	}
	if err := s.loadExecutions(ctx); err != nil {
		return err
	}
	s.releaseIdleAdmissions()
	if err := s.recoverExecutions(ctx, states); err != nil {
		return err
	}
	if err := s.cleanupInactiveProjects(ctx, states); err != nil {
		return err
	}
	if s.modelSettingsErr != nil {
		return s.modelSettingsErr
	}
	return s.admit(ctx, active)
}
