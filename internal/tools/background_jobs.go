package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// MaxBackgroundJobSeconds is the longest a background job may run: a full device test
	// suite with its emulator restarts, or a benchmark run, fits.
	MaxBackgroundJobSeconds = 4 * 60 * 60
	// jobTailBytes is how much of a job's latest output is kept in memory for reports; the
	// log file has all of it.
	jobTailBytes = 64 * 1024
	// finishedJobLines and finishedJobBytes bound the output a finish notice carries.
	finishedJobLines = 20
	finishedJobBytes = 4 * 1024
)

// shellProcess is a started run_command shell in its own process group. Its lifetime is its
// own rather than the turn's: the turn stops it by cancelling it, or hands it to the job
// registry to run on in the background.
type shellProcess struct {
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	timer    *time.Timer
	timedOut atomic.Bool
	stopped  atomic.Bool // stopped on request: a halted turn, check_command or /jobs
	done     chan struct{}
	err      error // cmd.Wait's error, set before done is closed
	out      *switchWriter
}

// startShell starts cmdStr under sh in workdir, writing its output to w, and kills its process
// group once timeout has passed.
func startShell(cmdStr, workdir, scratch string, w io.Writer, timeout time.Duration) (*shellProcess, error) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &shellProcess{cancel: cancel, done: make(chan struct{}), out: &switchWriter{w: w}}
	p.cmd = newRunCommand(ctx, cmdStr, workdir, scratch, timeout)
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	p.timer = time.AfterFunc(timeout, func() {
		p.timedOut.Store(true)
		cancel()
	})
	go func() {
		p.err = p.cmd.Wait()
		p.timer.Stop()
		cancel()
		close(p.done)
	}()
	return p, nil
}

// stop kills the process group and waits until the shell has been reaped.
func (p *shellProcess) stop() {
	p.stopped.Store(true)
	p.cancel()
	<-p.done
}

// extendTimeout lets the process run until limit after its start, as a background job may.
func (p *shellProcess) extendTimeout(started time.Time, limit time.Duration) {
	p.timer.Reset(max(time.Until(started.Add(limit)), 0))
}

// ending describes how a finished process ended: "exit 0", "exit 2", "timed out" or
// "stopped".
func (p *shellProcess) ending() string {
	switch {
	case p.timedOut.Load():
		return "timed out"
	case p.stopped.Load():
		return "stopped"
	case p.err == nil:
		return "exit 0"
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](p.err); ok && exitErr.ExitCode() >= 0 {
		return fmt.Sprintf("exit %d", exitErr.ExitCode())
	}
	return p.err.Error()
}

// switchWriter passes writes to one writer at a time, so a running command's output can move
// from the foreground result to a background job's log without losing a byte.
type switchWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *switchWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// swap replaces the writer with the one next returns; no write runs meanwhile.
func (s *switchWriter) swap(next func() io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.w = next()
}

// jobOutput is a background job's output: all of it in a log file, up to maxCommandLogBytes,
// and its latest jobTailBytes in memory for reports.
type jobOutput struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	logged int
	logErr error
	tail   []byte
	total  int
}

// Write keeps p in the log and the tail. It never fails: a log that cannot be written is
// noted in logErr and the tail is still kept.
func (o *jobOutput) Write(p []byte) (int, error) {
	o.append(p)
	return len(p), nil
}

func (o *jobOutput) append(p []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.total += len(p)
	if o.file != nil && o.logErr == nil {
		n, err := o.file.Write(p[:min(len(p), maxCommandLogBytes-o.logged)])
		o.logged += n
		o.logErr = err
	}
	o.tail = append(o.tail, p...)
	if len(o.tail) > 2*jobTailBytes {
		o.tail = append([]byte(nil), o.tail[len(o.tail)-jobTailBytes:]...)
	}
}

func (o *jobOutput) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.file != nil {
		if err := o.file.Close(); err != nil && o.logErr == nil {
			o.logErr = err
		}
		o.file = nil
	}
}

// lastLines returns up to n of the latest output lines, within maxBytes.
func (o *jobOutput) lastLines(n, maxBytes int) string {
	o.mu.Lock()
	text := strings.TrimRight(strings.ToValidUTF8(string(o.tail), ""), "\r\n")
	o.mu.Unlock()
	lines := strings.Split(text, "\n")
	lines = lines[max(0, len(lines)-n):]
	text = strings.Join(lines, "\n")
	if len(text) > maxBytes {
		text = text[len(text)-maxBytes:]
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:] // start on a whole line
		}
	}
	return text
}

// where names the log for a report: its path, and what went wrong with it.
func (o *jobOutput) where() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case o.path == "":
		return fmt.Sprintf("its output could not be saved: %v", o.logErr)
	case o.logErr != nil:
		return fmt.Sprintf("its output is in %s (saving stopped early: %v)", o.path, o.logErr)
	case o.logged < o.total:
		return fmt.Sprintf("its output is in %s (its first %s)", o.path, formatSize(o.logged))
	}
	return "its output is in " + o.path
}

// Job is a command running, or run, in the background.
type Job struct {
	ID      int
	Command string
	// Owner is the scratch directory of the conversation that started the job; the job's
	// finish is told to that conversation.
	Owner   string
	Started time.Time

	proc *shellProcess
	out  *jobOutput

	// Guarded by the registry's lock.
	ended    time.Time
	reported bool // the model has been told how the job ended
	noWake   bool // stopped by the user: told with their next message, not by waking
}

func (j *Job) done() bool {
	select {
	case <-j.proc.done:
		return true
	default:
		return false
	}
}

// headline is the job's first command line, cut to 80 characters.
func (j *Job) headline() string {
	line, _, more := strings.Cut(strings.TrimSpace(j.Command), "\n")
	if len(line) > 80 {
		line, more = strings.ToValidUTF8(line[:80], ""), true
	}
	if more {
		line = strings.TrimRight(line, " ") + " …"
	}
	return line
}

// JobRegistry holds the background jobs of this hmj process.
type JobRegistry struct {
	mu      sync.Mutex
	next    int
	jobs    []*Job
	changed chan struct{} // closed, and replaced, whenever a job ends
}

// Jobs is the process's job registry.
var Jobs = NewJobRegistry()

func NewJobRegistry() *JobRegistry {
	return &JobRegistry{changed: make(chan struct{})}
}

// start runs cmdStr as a background job of owner, the scratch directory of the conversation
// that asked for it.
func (r *JobRegistry) start(owner, cmdStr, workdir string, timeout time.Duration) (*Job, error) {
	j := r.newJob(owner, cmdStr)
	p, err := startShell(cmdStr, workdir, owner, j.out, timeout)
	if err != nil {
		j.out.close()
		if j.out.path != "" {
			if rmErr := os.Remove(j.out.path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				err = errors.Join(err, rmErr)
			}
		}
		return nil, err
	}
	j.Started, j.proc = time.Now(), p
	r.track(j)
	return j, nil
}

// adopt makes p, a running foreground command started at started, a background job of owner
// that may run until timeout after its start. Its log begins with what earlier returns, the
// output the foreground kept, taken while no output is written so nothing comes between.
func (r *JobRegistry) adopt(owner, cmdStr string, p *shellProcess, started time.Time, timeout time.Duration, earlier func() string) *Job {
	j := r.newJob(owner, cmdStr)
	p.out.swap(func() io.Writer {
		if text := earlier(); text != "" {
			j.out.append([]byte(text + "\n"))
		}
		return j.out
	})
	p.extendTimeout(started, timeout)
	j.Started, j.proc = started, p
	r.track(j)
	return j
}

// newJob numbers a job and opens its log, in the owner's scratch directory under jobs/, or
// beside the saved command outputs.
func (r *JobRegistry) newJob(owner, cmdStr string) *Job {
	r.mu.Lock()
	r.next++
	id := r.next
	r.mu.Unlock()

	out := &jobOutput{}
	dir := filepath.Join(owner, "jobs")
	if owner == "" {
		commandLogMu.Lock()
		dir = cmp.Or(commandLogDir, filepath.Join(os.TempDir(), "hmj-jobs"))
		commandLogMu.Unlock()
	}
	out.logErr = os.MkdirAll(dir, 0o700)
	if out.logErr == nil {
		path := filepath.Join(dir, fmt.Sprintf("job%d_%s_%s.log", id, time.Now().Format("20060102-150405"), commandLogName(cmdStr)))
		if out.file, out.logErr = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); out.logErr == nil {
			out.path = path
		}
	}
	return &Job{ID: id, Command: cmdStr, Owner: owner, out: out}
}

// track adds j to the registry and records its end when its process ends.
func (r *JobRegistry) track(j *Job) {
	r.mu.Lock()
	r.jobs = append(r.jobs, j)
	r.mu.Unlock()
	go func() {
		<-j.proc.done
		j.out.close()
		r.mu.Lock()
		j.ended = time.Now()
		close(r.changed)
		r.changed = make(chan struct{})
		r.mu.Unlock()
	}()
}

// get returns job id, or nil.
func (r *JobRegistry) get(id int) *Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, j := range r.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// waitEnded waits until the registry has recorded the end of j, whose process has ended.
func (r *JobRegistry) waitEnded(j *Job) {
	for {
		r.mu.Lock()
		ended, changed := !j.ended.IsZero(), r.changed
		r.mu.Unlock()
		if ended {
			return
		}
		<-changed
	}
}

// ids lists the jobs for an error naming the ones that exist.
func (r *JobRegistry) ids() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.jobs) == 0 {
		return "there are no background jobs"
	}
	names := make([]string, len(r.jobs))
	for i, j := range r.jobs {
		names[i] = fmt.Sprint(j.ID)
	}
	return "jobs: " + strings.Join(names, ", ")
}

// report describes job j for the model: a header with its state, up to lines of its latest
// output within maxBytes, and where the rest is. A finished job counts as reported.
func (r *JobRegistry) report(j *Job, lines, maxBytes int) string {
	r.mu.Lock()
	ended := j.ended
	finished := !ended.IsZero()
	if finished {
		j.reported = true
	}
	r.mu.Unlock()

	var header string
	if finished {
		header = fmt.Sprintf("[background job %d finished: %s after %s: %s]", j.ID, j.proc.ending(), formatElapsed(ended.Sub(j.Started)), j.headline())
	} else {
		header = fmt.Sprintf("[background job %d is running, for %s so far: %s]", j.ID, formatElapsed(time.Since(j.Started)), j.headline())
	}
	text := header
	if tail := j.out.lastLines(lines, maxBytes); tail != "" {
		text += "\n" + tail
	}
	return text + "\n[" + j.out.where() + "]"
}

// TakeFinished returns the finish notices of owner's jobs that ended since the model was last
// told, and counts them as told.
func (r *JobRegistry) TakeFinished(owner string) []string {
	r.mu.Lock()
	var due []*Job
	for _, j := range r.jobs {
		if j.Owner == owner && !j.ended.IsZero() && !j.reported {
			due = append(due, j)
		}
	}
	r.mu.Unlock()
	notices := make([]string, len(due))
	for i, j := range due {
		notices[i] = r.report(j, finishedJobLines, finishedJobBytes)
	}
	return notices
}

// Wake returns a channel that is closed once one of owner's jobs has ended without the model
// being told, at once if one has. It stops watching when stop is closed.
func (r *JobRegistry) Wake(owner string, stop <-chan struct{}) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for {
			r.mu.Lock()
			changed := r.changed
			due := false
			for _, j := range r.jobs {
				due = due || (j.Owner == owner && !j.ended.IsZero() && !j.reported && !j.noWake)
			}
			r.mu.Unlock()
			if due {
				close(ch)
				return
			}
			select {
			case <-changed:
			case <-stop:
				return
			}
		}
	}()
	return ch
}

// Running counts owner's jobs that are still running.
func (r *JobRegistry) Running(owner string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, j := range r.jobs {
		if j.Owner == owner && j.ended.IsZero() {
			n++
		}
	}
	return n
}

// JobLine is one job as /jobs lists it.
type JobLine struct {
	ID      int
	Running bool
	// Text is the job's number, state, time and command: "job 2  running 3m10s  ./gradlew …".
	Text string
}

// List describes every job, oldest first.
func (r *JobRegistry) List() []JobLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	lines := make([]JobLine, len(r.jobs))
	for i, j := range r.jobs {
		state := "running " + formatElapsed(time.Since(j.Started))
		if !j.ended.IsZero() {
			state = j.proc.ending() + " after " + formatElapsed(j.ended.Sub(j.Started))
		}
		lines[i] = JobLine{ID: j.ID, Running: j.ended.IsZero(), Text: fmt.Sprintf("job %d  %s  %s", j.ID, state, j.headline())}
	}
	return lines
}

// Stop stops job id, waiting until it has ended.
func (r *JobRegistry) Stop(id int) error {
	j := r.get(id)
	if j == nil {
		return fmt.Errorf("no background job %d; %s", id, r.ids())
	}
	if !j.done() {
		j.proc.stop()
	}
	return nil
}

// StopForUser stops job id for the user's /jobs stop. The model hears of it with the user's
// next message rather than in a turn of its own.
func (r *JobRegistry) StopForUser(id int) error {
	r.mu.Lock()
	for _, j := range r.jobs {
		if j.ID == id {
			j.noWake = true
		}
	}
	r.mu.Unlock()
	return r.Stop(id)
}

// StopAll stops every running job, as hmj exits.
func (r *JobRegistry) StopAll() {
	r.mu.Lock()
	jobs := append([]*Job(nil), r.jobs...)
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, j := range jobs {
		if !j.done() {
			wg.Go(j.proc.stop)
		}
	}
	wg.Wait()
}

// formatElapsed writes a duration in whole seconds as the status line does: 42s, 2m05s or
// 1h02m.
func formatElapsed(d time.Duration) string {
	sec := int(d.Round(time.Second).Seconds())
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	default:
		return fmt.Sprintf("%dh%02dm", sec/3600, sec%3600/60)
	}
}

type backgroundKeyKey struct{}

// WithBackgroundKey returns a copy of ctx that carries keys, which receives when the user
// presses Ctrl+B: a foreground run_command then moves to the background as a job.
func WithBackgroundKey(ctx context.Context, keys <-chan struct{}) context.Context {
	return context.WithValue(ctx, backgroundKeyKey{}, keys)
}

// backgroundKeyFrom returns the Ctrl+B channel ctx carries, or nil, which never receives.
func backgroundKeyFrom(ctx context.Context) <-chan struct{} {
	keys, _ := ctx.Value(backgroundKeyKey{}).(<-chan struct{})
	return keys
}
