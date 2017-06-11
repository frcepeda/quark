package grader

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/inconshreveable/log15"
	"github.com/lhchavez/quark/common"
	"github.com/lhchavez/quark/runner"
	"io/ioutil"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"
)

type QueuePriority int

const (
	QueuePriorityHigh   = QueuePriority(0)
	QueuePriorityNormal = QueuePriority(1)
	QueuePriorityLow    = QueuePriority(2)
)

// RunInfo holds the necessary data of a Run, even after the RunContext is
// gone.
type RunInfo struct {
	ID          int64
	GUID        string
	Contest     *string
	ProblemName string
	Run         *common.Run
	Result      runner.RunResult
	GradeDir    string
	Priority    QueuePriority
	PenaltyType string

	CreationTime time.Time
}

// RunContext is a wrapper around a RunInfo. This is used when a Run is sitting
// on a Queue on the grader.
type RunContext struct {
	RunInfo

	// These fields are there so that the RunContext can be used as a normal
	// Context.
	Log            log15.Logger
	EventCollector common.EventCollector
	EventFactory   *common.EventFactory
	Config         *common.Config

	// A flag to be able to atomically close the RunContext exactly once.
	closed int32
	// A reference to the Input so that it is not evicted while RunContext is
	// still active
	input common.Input

	tries   int
	queue   *Queue
	context *common.Context
	monitor *InflightMonitor

	// A channel that will be closed once the run is ready.
	ready chan struct{}
}

// AddRunContext registers a RunContext into the grader.
func AddRunContext(
	ctx *Context,
	run *RunContext,
	input common.Input,
) error {
	run.input = input
	run.context = ctx.Context.DebugContext("id", run.ID)

	run.Config = &run.context.Config
	run.Log = run.context.Log
	run.EventCollector = run.context.EventCollector
	run.EventFactory = run.context.EventFactory

	return nil
}

func NewEmptyRunContext(ctx *Context) *RunContext {
	return &RunContext{
		RunInfo: RunInfo{
			Run: &common.Run{
				AttemptID: common.NewAttemptID(),
				MaxScore:  1.0,
			},
			Result: runner.RunResult{
				Verdict: "JE",
			},
			CreationTime: time.Now(),
			Priority:     QueuePriorityNormal,
		},
		tries: ctx.Config.Grader.MaxGradeRetries,
		ready: make(chan struct{}),
	}
}

func (run *RunContext) Debug() error {
	var err error
	if run.GradeDir, err = ioutil.TempDir("", "grade"); err != nil {
		return err
	}
	run.Run.Debug = true
	return nil
}

func (run *RunContext) Close() {
	if atomic.SwapInt32(&run.closed, 1) != 0 {
		run.Log.Warn("Attempting to close an already closed run")
		return
	}
	var postProcessor *RunPostProcessor = nil
	if run.monitor != nil {
		postProcessor = run.monitor.PostProcessor
		run.monitor.Remove(run.Run.AttemptID)
	}
	if run.input != nil {
		run.input.Release(run.input)
		run.input = nil
	}
	if err := os.MkdirAll(run.GradeDir, 0755); err != nil {
		run.Log.Error("Unable to create grade dir", "err", err)
		return
	}

	// Results
	{
		fd, err := os.Create(path.Join(run.GradeDir, "details.json"))
		if err != nil {
			run.Log.Error("Unable to create details.json file", "err", err)
			return
		}
		defer fd.Close()
		prettyPrinted, err := json.MarshalIndent(&run.Result, "", "  ")
		if err != nil {
			run.Log.Error("Unable to marshal results file", "err", err)
			return
		}
		if _, err := fd.Write(prettyPrinted); err != nil {
			run.Log.Error("Unable to write results file", "err", err)
			return
		}
	}

	// Persist logs
	{
		fd, err := os.Create(path.Join(run.GradeDir, "logs.txt.gz"))
		if err != nil {
			run.Log.Error("Unable to create log file", "err", err)
			return
		}
		defer fd.Close()
		gz := gzip.NewWriter(fd)
		if _, err := gz.Write(run.context.LogBuffer()); err != nil {
			run.Log.Error("Unable to write log file", "err", err)
			return
		}
		if err := gz.Close(); err != nil {
			run.Log.Error("Unable to finalize log file", "err", err)
			return
		}
	}

	// Persist tracing info
	{
		fd, err := os.Create(path.Join(run.GradeDir, "tracing.json.gz"))
		if err != nil {
			run.Log.Error("Unable to create tracing file", "err", err)
			return
		}
		defer fd.Close()
		gz := gzip.NewWriter(fd)
		if _, err := gz.Write(run.context.TraceBuffer()); err != nil {
			run.Log.Error("Unable to upload traces", "err", err)
			return
		}
		if err := gz.Close(); err != nil {
			run.Log.Error("Unable to finalize traces", "err", err)
			return
		}
	}

	close(run.ready)
	if postProcessor != nil {
		postProcessor.PostProcess(&run.RunInfo)
	}
}

func (run *RunContext) AppendRunnerLogs(runnerName string, contents []byte) {
	run.context.AppendLogSection(runnerName, contents)
}

// Requeue adds a RunContext back to the Queue from where it came from, if it
// has any retries left. It always adds the RunContext to the highest-priority
// queue.
func (run *RunContext) Requeue(lastAttempt bool) bool {
	if run.monitor != nil {
		run.monitor.Remove(run.Run.AttemptID)
	}
	run.tries -= 1
	if run.tries <= 0 {
		run.Close()
		return false
	}
	if lastAttempt {
		// If this is the result of a runner successfully sending a JE verdict, it
		// _might_ be a transient problem. In any case, only attempt to run it at
		// most once more.
		run.tries = 1
	}
	run.Run.UpdateAttemptID()
	// Since it was already ready to be executed, place it in the high-priority
	// queue.
	if !run.queue.enqueue(run, QueuePriorityHigh) {
		// That queue is full. We've exhausted all our options, bail out.
		run.Close()
		return false
	}
	return true
}

func (run *RunContext) String() string {
	return fmt.Sprintf(
		"RunContext{ID:%d GUID:%s ProblemName:%s, %s}",
		run.ID,
		run.GUID,
		run.ProblemName,
		run.Run,
	)
}

func (run *RunContext) Ready() <-chan struct{} {
	return run.ready
}

// Queue represents a RunContext queue with three discrete priorities.
type Queue struct {
	Name  string
	runs  [3]chan *RunContext
	ready chan struct{}
}

// GetRun dequeues a RunContext from the queue and adds it to the global
// InflightMonitor. This function will block if there are no RunContext objects
// in the queue.
func (queue *Queue) GetRun(
	runner string,
	monitor *InflightMonitor,
	closeNotifier <-chan bool,
) (*RunContext, <-chan struct{}, bool) {
	select {
	case <-closeNotifier:
		return nil, nil, false
	case <-queue.ready:
	}

	for i := range queue.runs {
		select {
		case run := <-queue.runs[i]:
			inflight := monitor.Add(run, runner)
			return run, inflight.timeout, true
		default:
		}
	}
	panic("unreachable")
}

func (queue *Queue) AddRun(run *RunContext) {
	// TODO(lhchavez): Add async events for queue operations.
	// Add new runs to the normal priority by default.
	queue.enqueueBlocking(run)
}

// enqueueBlocking adds a run to the queue, waits if needed.
func (queue *Queue) enqueueBlocking(run *RunContext) {
	if run == nil {
		panic("null RunContext")
	}
	run.queue = queue
	queue.runs[run.Priority] <- run
	queue.ready <- struct{}{}
}

// enqueue adds a run to the queue, returns true if possible.
func (queue *Queue) enqueue(run *RunContext, priority QueuePriority) bool {
	if run == nil {
		panic("null RunContext")
	}
	run.queue = queue
	select {
	case queue.runs[priority] <- run:
		queue.ready <- struct{}{}
		return true
	default:
		// There is no space left in the queue.
		return false
	}
}

// InflightRun is a wrapper around a RunContext when it is handed off a queue
// and a runner has been assigned to it.
type InflightRun struct {
	run          *RunContext
	runner       string
	creationTime time.Time
	connected    chan struct{}
	ready        chan struct{}
	timeout      chan struct{}
}

// InflightMonitor manages all in-flight Runs (Runs that have been picked up by
// a runner) and tracks their state in case the runner becomes unresponsive.
type InflightMonitor struct {
	sync.Mutex
	PostProcessor  *RunPostProcessor
	mapping        map[uint64]*InflightRun
	connectTimeout time.Duration
	readyTimeout   time.Duration
}

// RunData represents the data of a single run.
type RunData struct {
	AttemptID    uint64
	ID           int64
	GUID         string
	Queue        string
	AttemptsLeft int
	Runner       string
	Time         int64
	Elapsed      int64
}

func NewInflightMonitor() *InflightMonitor {
	monitor := &InflightMonitor{
		PostProcessor:  NewRunPostProcessor(),
		mapping:        make(map[uint64]*InflightRun),
		connectTimeout: time.Duration(10) * time.Minute,
		readyTimeout:   time.Duration(10) * time.Minute,
	}
	go monitor.PostProcessor.run()
	return monitor
}

// Add creates an InflightRun wrapper for the specified RunContext, adds it to
// the InflightMonitor, and monitors it for timeouts. A RunContext can be later
// accesssed through its attempt ID.
func (monitor *InflightMonitor) Add(
	run *RunContext,
	runner string,
) *InflightRun {
	monitor.Lock()
	defer monitor.Unlock()
	inflight := &InflightRun{
		run:          run,
		runner:       runner,
		creationTime: time.Now(),
		connected:    make(chan struct{}, 1),
		ready:        make(chan struct{}, 1),
		timeout:      make(chan struct{}, 1),
	}
	run.monitor = monitor
	monitor.mapping[run.Run.AttemptID] = inflight
	go func() {
		select {
		case <-inflight.connected:
			select {
			case <-inflight.ready:
			case <-time.After(monitor.readyTimeout):
				monitor.timeout(run, inflight.timeout)
			}
		case <-time.After(monitor.connectTimeout):
			monitor.timeout(run, inflight.timeout)
		}
		close(inflight.timeout)
	}()
	return inflight
}

func (monitor *InflightMonitor) timeout(
	run *RunContext,
	timeout chan<- struct{},
) {
	run.context.Log.Error("run timed out. retrying", "context", run)
	if !run.Requeue(false) {
		run.context.Log.Error("run timed out too many times. giving up")
	}
	timeout <- struct{}{}
}

// Get returns the RunContext associated with the specified attempt ID.
func (monitor *InflightMonitor) Get(attemptID uint64) (*RunContext, <-chan struct{}, bool) {
	monitor.Lock()
	defer monitor.Unlock()
	inflight, ok := monitor.mapping[attemptID]
	if ok {
		// Try to signal that the runner has connected, unless it was already
		// signalled before.
		select {
		case inflight.connected <- struct{}{}:
		default:
		}
		return inflight.run, inflight.timeout, ok
	}
	return nil, nil, ok
}

// Remove removes the specified attempt ID from the in-flight runs and signals
// the RunContext for completion.
func (monitor *InflightMonitor) Remove(attemptID uint64) {
	monitor.Lock()
	defer monitor.Unlock()
	inflight, ok := monitor.mapping[attemptID]
	if ok {
		inflight.run.monitor = nil
		select {
		// Try to signal that the run has been connected.
		case inflight.connected <- struct{}{}:
		default:
		}
		select {
		// Try to signal that the run has been finished.
		case inflight.ready <- struct{}{}:
		default:
		}
	}
	delete(monitor.mapping, attemptID)
}

func (monitor *InflightMonitor) GetRunData() []*RunData {
	monitor.Lock()
	defer monitor.Unlock()

	data := make([]*RunData, len(monitor.mapping))
	idx := 0
	now := time.Now()
	for attemptId, inflight := range monitor.mapping {
		data[idx] = &RunData{
			AttemptID:    attemptId,
			ID:           inflight.run.ID,
			GUID:         inflight.run.GUID,
			Queue:        inflight.run.queue.Name,
			AttemptsLeft: inflight.run.tries,
			Runner:       inflight.runner,
			Time:         inflight.creationTime.Unix(),
			Elapsed:      now.Sub(inflight.creationTime).Nanoseconds(),
		}
		idx += 1
	}

	return data
}

func (monitor *InflightMonitor) MarshalJSON() ([]byte, error) {
	return json.MarshalIndent(monitor.GetRunData(), "", "  ")
}

type runPostProcessorListener struct {
	listener *chan<- *RunInfo
	added    *chan struct{}
}

type RunPostProcessor struct {
	finishedRuns chan *RunInfo
	listenerChan chan runPostProcessorListener
	listeners    []chan<- *RunInfo
}

func NewRunPostProcessor() *RunPostProcessor {
	return &RunPostProcessor{
		finishedRuns: make(chan *RunInfo, 1),
		listenerChan: make(chan runPostProcessorListener, 1),
		listeners:    make([]chan<- *RunInfo, 0),
	}
}

func (postProcessor *RunPostProcessor) AddListener(c chan<- *RunInfo) {
	added := make(chan struct{}, 0)
	postProcessor.listenerChan <- runPostProcessorListener{
		listener: &c,
		added:    &added,
	}
	select {
	case <-added:
	}
}

func (postProcessor *RunPostProcessor) PostProcess(run *RunInfo) {
	postProcessor.finishedRuns <- run
}

func (postProcessor *RunPostProcessor) run() {
	for {
		select {
		case wrappedListener := <-postProcessor.listenerChan:
			postProcessor.listeners = append(
				postProcessor.listeners,
				*wrappedListener.listener,
			)
			close(*wrappedListener.added)
		case run, ok := <-postProcessor.finishedRuns:
			if !ok {
				for _, listener := range postProcessor.listeners {
					close(listener)
				}
				return
			}
			for _, listener := range postProcessor.listeners {
				listener <- run
			}
		}
	}
}

func (postProcessor *RunPostProcessor) Close() {
	close(postProcessor.finishedRuns)
}

// QueueManager is an expvar-friendly manager for Queues.
type QueueManager struct {
	sync.Mutex
	mapping       map[string]*Queue
	channelLength int
}

// QueueInfo has information about one queue.
type QueueInfo struct {
	Lengths [3]int
}

func NewQueueManager(channelLength int) *QueueManager {
	manager := &QueueManager{
		mapping:       make(map[string]*Queue),
		channelLength: channelLength,
	}
	manager.Add("default")
	return manager
}

func (manager *QueueManager) Add(name string) *Queue {
	queue := &Queue{
		Name:  name,
		ready: make(chan struct{}, 3*manager.channelLength),
	}
	for r := range queue.runs {
		queue.runs[r] = make(chan *RunContext, manager.channelLength)
	}
	manager.Lock()
	defer manager.Unlock()
	manager.mapping[name] = queue
	return queue
}

func (manager *QueueManager) Get(name string) (*Queue, error) {
	manager.Lock()
	defer manager.Unlock()

	queue, ok := manager.mapping[name]
	if !ok {
		return nil, errors.New(fmt.Sprintf("cannot find queue %q", name))
	}
	return queue, nil
}

func (manager *QueueManager) GetQueueInfo() map[string]QueueInfo {
	manager.Lock()
	defer manager.Unlock()

	queues := make(map[string]QueueInfo)
	for name, queue := range manager.mapping {
		queues[name] = QueueInfo{
			Lengths: [3]int{
				len(queue.runs[0]),
				len(queue.runs[1]),
				len(queue.runs[2]),
			},
		}
	}
	return queues
}

func (manager *QueueManager) MarshalJSON() ([]byte, error) {
	return json.MarshalIndent(manager.GetQueueInfo(), "", "  ")
}
