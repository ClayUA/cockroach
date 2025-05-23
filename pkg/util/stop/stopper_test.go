// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package stop_test

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/quotapool"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

func TestStopper(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := stop.NewStopper()
	running := make(chan struct{})
	waiting := make(chan struct{})
	cleanup := make(chan struct{})
	ctx := context.Background()

	_ = s.RunAsyncTask(ctx, "task", func(context.Context) {
		<-running
	})

	go func() {
		s.Stop(ctx)
		close(waiting)
		<-cleanup
	}()

	<-s.ShouldQuiesce()
	select {
	case <-waiting:
		close(cleanup)
		t.Fatal("expected stopper to have blocked")
	case <-time.After(100 * time.Millisecond):
		// Expected.
	}
	close(running)
	select {
	case <-waiting:
		// Success.
	case <-time.After(time.Second):
		close(cleanup)
		t.Fatal("stopper should have finished waiting")
	}
	close(cleanup)
}

type blockingCloser struct {
	block chan struct{}
}

func newBlockingCloser() *blockingCloser {
	return &blockingCloser{block: make(chan struct{})}
}

func (bc *blockingCloser) Unblock() {
	close(bc.block)
}

func (bc *blockingCloser) Close() {
	<-bc.block
}

func TestStopperIsStopped(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := stop.NewStopper()
	bc := newBlockingCloser()
	s.AddCloser(bc)
	go s.Stop(context.Background())

	select {
	case <-s.ShouldQuiesce():
	case <-time.After(time.Second):
		t.Fatal("stopper should have finished waiting")
	}
	select {
	case <-s.IsStopped():
		t.Fatal("expected blocked closer to prevent stop")
	case <-time.After(100 * time.Millisecond):
		// Expected.
	}
	bc.Unblock()
	select {
	case <-s.IsStopped():
		// Expected
	case <-time.After(time.Second):
		t.Fatal("stopper should have finished stopping")
	}

	s.Stop(context.Background())
}

func TestStopperMultipleTasks(t *testing.T) {
	defer leaktest.AfterTest(t)()
	const count = 3
	s := stop.NewStopper()
	ctx := context.Background()

	for i := 0; i < count; i++ {
		require.NoError(t, s.RunAsyncTask(ctx, "task", func(context.Context) {
			<-s.ShouldQuiesce()
		}))
	}

	done := make(chan struct{})
	go func() {
		s.Stop(ctx)
		close(done)
	}()

	<-done
}

func TestStopperStartFinishTasks(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	s := stop.NewStopper()
	defer s.Stop(ctx)

	if err := s.RunTask(ctx, "test", func(ctx context.Context) {
		go s.Stop(ctx)

		select {
		case <-s.IsStopped():
			t.Fatal("stopper not fully stopped")
		case <-time.After(100 * time.Millisecond):
			// Expected.
		}
	}); err != nil {
		t.Error(err)
	}
	select {
	case <-s.IsStopped():
		// Success.
	case <-time.After(time.Second):
		t.Fatal("stopper should be ready to stop")
	}
}

// TestStopperQuiesce tests coordinate quiesce with Quiesce.
func TestStopperQuiesce(t *testing.T) {
	defer leaktest.AfterTest(t)()
	var stoppers []*stop.Stopper
	for i := 0; i < 3; i++ {
		stoppers = append(stoppers, stop.NewStopper())
	}
	var quiesceDone []chan struct{}
	var runTaskDone []chan struct{}
	ctx := context.Background()

	for _, s := range stoppers {
		thisStopper := s
		qc := make(chan struct{})
		quiesceDone = append(quiesceDone, qc)
		sc := make(chan struct{})
		runTaskDone = append(runTaskDone, sc)
		go func() {
			// Wait until Quiesce() is called.
			<-qc
			err := thisStopper.RunTask(ctx, "inner", func(context.Context) {})
			if !errors.HasType(err, (*kvpb.NodeUnavailableError)(nil)) {
				t.Error(err)
			}
			// Make the stoppers call Stop().
			close(sc)
			<-thisStopper.ShouldQuiesce()
		}()
	}

	done := make(chan struct{})
	go func() {
		for _, s := range stoppers {
			s.Quiesce(ctx)
		}
		// Make the tasks call RunTask().
		for _, qc := range quiesceDone {
			close(qc)
		}

		// Wait until RunTask() is called.
		for _, sc := range runTaskDone {
			<-sc
		}

		for _, s := range stoppers {
			s.Stop(ctx)
			<-s.IsStopped()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Errorf("timed out waiting for stop")
	}
}

type testCloser bool

func (tc *testCloser) Close() {
	*tc = true
}

func TestStopperClosers(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := stop.NewStopper()
	x := 1
	s.AddCloser(stop.CloserFn(func() { x++ }))
	s.AddCloser(stop.CloserFn(func() { x *= 10 }))
	s.Stop(context.Background())
	// Both closers should run in LIFO order, so we should get 11 (and not 20).
	if expected := 11; x != expected {
		t.Errorf("expected %d, got %d", expected, x)
	}
}

func TestStopperCloserConcurrent(t *testing.T) {
	defer leaktest.AfterTest(t)()
	const trials = 10
	for i := 0; i < trials; i++ {
		s := stop.NewStopper()
		var tc1 testCloser

		// Add Closer and Stop concurrently. There should be
		// no circumstance where the Closer is not called.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			runtime.Gosched()
			s.AddCloser(&tc1)
		}()
		go func() {
			defer wg.Done()
			runtime.Gosched()
			s.Stop(context.Background())
		}()
		wg.Wait()

		if !tc1 {
			t.Errorf("expected true; got %t", tc1)
		}
	}
}

func TestStopperNumTasks(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := stop.NewStopper()
	ctx := context.Background()
	defer s.Stop(ctx)
	var tasks []chan bool
	for i := 0; i < 3; i++ {
		c := make(chan bool)
		tasks = append(tasks, c)
		if err := s.RunAsyncTask(ctx, "test", func(_ context.Context) {
			// Wait for channel to close
			<-c
		}); err != nil {
			t.Fatal(err)
		}
		if numTasks := s.NumTasks(); numTasks != i+1 {
			t.Errorf("stopper should have %d running tasks, got %d", i+1, numTasks)
		}
	}
	for i, c := range tasks {
		// Close the channel to let the task proceed.
		close(c)
		expNum := len(tasks[i+1:])
		testutils.SucceedsSoon(t, func() error {
			if nt := s.NumTasks(); nt != expNum {
				return errors.Errorf("%d: stopper should have %d running tasks, got %d", i, expNum, nt)
			}
			return nil
		})
	}
}

func TestStopperWithCancel(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := stop.NewStopper()
	ctx := context.Background()
	ctx1, _ := s.WithCancelOnQuiesce(ctx)
	ctx3, cancel3 := s.WithCancelOnQuiesce(ctx)

	if err := ctx1.Err(); err != nil {
		t.Fatalf("should not be canceled: %v", err)
	}
	if err := ctx3.Err(); err != nil {
		t.Fatalf("should not be canceled: %v", err)
	}

	cancel3()
	if err := ctx1.Err(); err != nil {
		t.Fatalf("should not be canceled: %v", err)
	}
	if err := ctx3.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("should be canceled: %v", err)
	}

	s.Quiesce(ctx)
	if err := ctx1.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("should be canceled: %v", err)
	}

	s.Stop(ctx)
}

func TestStopperWithCancelConcurrent(t *testing.T) {
	defer leaktest.AfterTest(t)()
	const trials = 10
	for i := 0; i < trials; i++ {
		s := stop.NewStopper()
		ctx := context.Background()
		var ctx1 context.Context

		// Tie a context to the Stopper and Stop concurrently. There should
		// be no circumstance where either Context is not canceled.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			runtime.Gosched()
			ctx1, _ = s.WithCancelOnQuiesce(ctx)
		}()
		go func() {
			defer wg.Done()
			runtime.Gosched()
			s.Stop(ctx)
		}()
		wg.Wait()

		if err := ctx1.Err(); !errors.Is(err, context.Canceled) {
			t.Errorf("should be canceled: %v", err)
		}
	}
}

func TestStopperShouldQuiesce(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := stop.NewStopper()
	runningTask := make(chan struct{})
	waiting := make(chan struct{})
	cleanup := make(chan struct{})
	ctx := context.Background()

	// Run an asynchronous task. A stopper which has been Stop()ed will not
	// close it's ShouldStop() channel until all tasks have completed. This task
	// will complete when the "runningTask" channel is closed.
	if err := s.RunAsyncTask(ctx, "test", func(_ context.Context) {
		<-runningTask
	}); err != nil {
		t.Fatal(err)
	}

	go func() {
		s.Stop(ctx)
		close(waiting)
		<-cleanup
	}()

	// The ShouldQuiesce() channel should close as soon as the stopper is
	// Stop()ed.
	<-s.ShouldQuiesce()
	// After completing the running task, the ShouldStop() channel should
	// now close.
	close(runningTask)
	select {
	case <-s.IsStopped():
	// Good.
	case <-time.After(10 * time.Second):
		t.Fatal("stopper did not fully stop in time")
	}
	<-waiting
	close(cleanup)
}

func TestStopperRunLimitedAsyncTask(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := stop.NewStopper()
	defer s.Stop(context.Background())

	const maxConcurrency = 5
	const numTasks = maxConcurrency * 3
	sem := quotapool.NewIntPool("test", maxConcurrency)
	taskSignal := make(chan struct{}, maxConcurrency)
	var mu syncutil.Mutex
	concurrency := 0
	peakConcurrency := 0
	var wg sync.WaitGroup

	f := func(_ context.Context) {
		mu.Lock()
		concurrency++
		if concurrency > peakConcurrency {
			peakConcurrency = concurrency
		}
		mu.Unlock()
		<-taskSignal
		mu.Lock()
		concurrency--
		mu.Unlock()
		wg.Done()
	}
	go func() {
		// Loop until the desired peak concurrency has been reached.
		for {
			mu.Lock()
			c := concurrency
			mu.Unlock()
			if c >= maxConcurrency {
				break
			}
			time.Sleep(time.Millisecond)
		}
		// Then let the rest of the async tasks finish quickly.
		for i := 0; i < numTasks; i++ {
			taskSignal <- struct{}{}
		}
	}()

	for i := 0; i < numTasks; i++ {
		wg.Add(1)
		if err := s.RunAsyncTaskEx(
			context.Background(),
			stop.TaskOpts{
				TaskName:   "test",
				Sem:        sem,
				WaitForSem: true,
			},
			f,
		); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if concurrency != 0 {
		t.Fatalf("expected 0 concurrency at end of test but got %d", concurrency)
	}
	if peakConcurrency != maxConcurrency {
		t.Fatalf("expected peak concurrency %d to equal max concurrency %d",
			peakConcurrency, maxConcurrency)
	}

	sem = quotapool.NewIntPool("test", 1)
	_, err := sem.Acquire(context.Background(), 1)
	require.NoError(t, err)
	err = s.RunAsyncTaskEx(
		context.Background(),
		stop.TaskOpts{
			TaskName:   "test",
			Sem:        sem,
			WaitForSem: false,
		},
		func(_ context.Context) {},
	)
	if !errors.Is(err, stop.ErrThrottled) {
		t.Fatalf("expected %v; got %v", stop.ErrThrottled, err)
	}
}

// This test ensures that if a quotapool has been registered as a Closer for
// the stopper and the stopper is Quiesced then blocked tasks will return
// ErrUnavailable.
func TestStopperRunLimitedAsyncTaskCloser(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	s := stop.NewStopper()
	defer s.Stop(ctx)

	sem := quotapool.NewIntPool("test", 1)
	s.AddCloser(sem.Closer("stopper"))
	_, err := sem.Acquire(ctx, 1)
	require.NoError(t, err)
	go func() {
		time.Sleep(time.Millisecond)
		s.Stop(ctx)
	}()
	err = s.RunAsyncTaskEx(ctx,
		stop.TaskOpts{
			TaskName:   "foo",
			Sem:        sem,
			WaitForSem: true,
		},
		func(context.Context) {})
	require.Equal(t, stop.ErrUnavailable, err)
	<-s.IsStopped()
}

func TestStopperRunLimitedAsyncTaskCancelContext(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := stop.NewStopper()
	defer s.Stop(context.Background())

	const maxConcurrency = 5
	sem := quotapool.NewIntPool("test", maxConcurrency)

	// Synchronization channels.
	workersDone := make(chan struct{})
	workerStarted := make(chan struct{})

	var workersRun int32
	var workersCanceled int32

	ctx, cancel := context.WithCancel(context.Background())
	f := func(ctx context.Context) {
		atomic.AddInt32(&workersRun, 1)
		workerStarted <- struct{}{}
		<-ctx.Done()
	}

	// This loop will block when the semaphore is filled.
	if err := s.RunAsyncTask(ctx, "test", func(ctx context.Context) {
		for i := 0; i < maxConcurrency*2; i++ {
			if err := s.RunAsyncTaskEx(ctx,
				stop.TaskOpts{
					TaskName:   "test",
					Sem:        sem,
					WaitForSem: true,
				},
				f); err != nil {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				atomic.AddInt32(&workersCanceled, 1)
			}
		}
		close(workersDone)
	}); err != nil {
		t.Fatal(err)
	}

	// Ensure that the semaphore fills up, leaving maxConcurrency workers
	// waiting for context cancelation.
	for i := 0; i < maxConcurrency; i++ {
		<-workerStarted
	}

	// Cancel the context, which should result in all subsequent attempts to
	// queue workers failing.
	cancel()
	<-workersDone

	if a, e := atomic.LoadInt32(&workersRun), int32(maxConcurrency); a != e {
		t.Fatalf("%d workers ran before context close, expected exactly %d", a, e)
	}
	if a, e := atomic.LoadInt32(&workersCanceled), int32(maxConcurrency); a != e {
		t.Fatalf("%d workers canceled after context close, expected exactly %d", a, e)
	}
}

func TestCancelInCloser(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	s := stop.NewStopper()
	defer s.Stop(ctx)

	// This will call the Closer which will call cancel and should
	// not deadlock.
	_, cancel := s.WithCancelOnQuiesce(ctx)
	s.AddCloser(closerFunc(cancel))
	s.Stop(ctx)
}

// closerFunc implements Closer.
type closerFunc func()

func (cf closerFunc) Close() { cf() }

// Test that task spans are included or not in the parent's recording based on
// the ChildSpan option.
func TestStopperRunAsyncTaskTracing(t *testing.T) {
	defer leaktest.AfterTest(t)()
	tr := tracing.NewTracerWithOpt(context.Background(), tracing.WithTracingMode(tracing.TracingModeActiveSpansRegistry))
	s := stop.NewStopper(stop.WithTracer(tr))

	ctx, getRecAndFinish := tracing.ContextWithRecordingSpan(context.Background(), tr, "parent")
	defer getRecAndFinish()
	root := tracing.SpanFromContext(ctx)
	require.NotNil(t, root)
	traceID := root.TraceID()

	// Start two child tasks. Only the one with ChildSpan:true is expected to be
	// present in the parent's recording.
	require.NoError(t, s.RunAsyncTaskEx(ctx, stop.TaskOpts{
		TaskName: "async child different recording",
		SpanOpt:  stop.FollowsFromSpan,
	},
		func(ctx context.Context) {
			log.Event(ctx, "async 1")
		},
	))
	require.NoError(t, s.RunAsyncTaskEx(ctx, stop.TaskOpts{
		TaskName: "async child same trace",
		SpanOpt:  stop.ChildSpan,
	},
		func(ctx context.Context) {
			log.Event(ctx, "async 2")
		},
	))

	{
		errC := make(chan error)
		require.NoError(t, s.RunAsyncTaskEx(ctx, stop.TaskOpts{
			TaskName: "sterile parent",
			SpanOpt:  stop.SterileRootSpan,
		},
			func(ctx context.Context) {
				log.Event(ctx, "async 3")
				sp1 := tracing.SpanFromContext(ctx)
				if sp1 == nil {
					errC <- errors.Errorf("missing span")
					return
				}
				sp2 := tr.StartSpan("child", tracing.WithParent(sp1))
				defer sp2.Finish()
				if sp2.TraceID() == traceID {
					errC <- errors.Errorf("expected different trace")
				}
				close(errC)
			},
		))
		require.NoError(t, <-errC)
	}

	s.Stop(ctx)
	require.NoError(t, tracing.CheckRecordedSpans(getRecAndFinish(), `
		span: parent
			tags: _verbose=1
			span: async child same trace
				tags: _verbose=1
				event: async 2`))
}

// Test that RunAsyncTask creates root spans only if the caller has a
// span.
func TestStopperRunAsyncTaskCreatesRootSpans(t *testing.T) {
	defer leaktest.AfterTest(t)()

	testutils.RunTrueAndFalse(t, "hasSpan", func(t *testing.T, hasSpan bool) {
		tr := tracing.NewTracer()
		ctx := context.Background()
		s := stop.NewStopper(stop.WithTracer(tr))
		defer s.Stop(ctx)
		c := make(chan *tracing.Span)
		if hasSpan {
			var sp *tracing.Span
			ctx, sp = tr.StartSpanCtx(ctx, "root", tracing.WithForceRealSpan())
			defer sp.Finish()
		}
		require.NoError(t, s.RunAsyncTask(ctx, "test",
			func(ctx context.Context) {
				c <- tracing.SpanFromContext(ctx)
			},
		))
		require.Equal(t, hasSpan, <-c != nil)
	})
}

// TestHandleOnFinishedTraceSpan shows that we can successfully use the Stopper
// in situations in which the trace span gets finished after creating the Handle
// but before launching the task.
func TestHandleOnFinishedTraceSpan(t *testing.T) {
	defer leaktest.AfterTest(t)()
	tr := tracing.NewTracer()
	ctx := context.Background()
	s := stop.NewStopper(stop.WithTracer(tr))
	defer s.Stop(ctx)

	ctx, sp := tr.StartSpanCtx(ctx, "root", tracing.WithForceRealSpan())
	// NB: can't `defer sp.Finish()` here since we'd then double-finish the span
	// in this test and trip the assertion.
	ctx, hdl, err := s.GetHandle(ctx, stop.TaskOpts{
		TaskName: "test",
		SpanOpt:  stop.ChildSpan,
	})
	require.NoError(t, err)

	sp.Finish()
	go func(ctx context.Context, hdl *stop.Handle) {
		defer hdl.Activate(ctx).Release(ctx)
	}(ctx, hdl)
}

// TestHandleWithoutActivateOrRelease demonstrates that acquiring a Handle
// without releasing it blocks the stopper on drain, meaning that such mistakes
// are likely to be caught quickly.
func TestHandleWithoutActivateOrRelease(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()

	testutils.RunTrueAndFalse(t, "activate", func(t *testing.T, activate bool) {
		s := stop.NewStopper()
		defer s.Stop(ctx)

		ctx, hdl, err := s.GetHandle(ctx, stop.TaskOpts{})
		require.NoError(t, err)

		relCh := make(chan stop.ActiveHandle, 1)
		go func() {
			if activate {
				relCh <- hdl.Activate(ctx)
				// Oops, forgot release!
			} // otherwise, forgot Activate too
		}()

		stopCh := make(chan struct{})
		go func() {
			s.Quiesce(ctx)
			close(stopCh)
		}()
		select {
		case <-time.After(time.Millisecond):
			// Expected: we acquired a handle, but never released it.
		case <-stopCh:
			t.Fatal("stopper unexpectedly quiesced")
		}
		// Release the handle, allowing the deferred Stop to work.
		if !activate {
			hdl.Activate(ctx).Release(ctx)
		} else {
			(<-relCh).Release(ctx)
		}
	})
}
