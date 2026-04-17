package gc

import (
	"context"
	"sync"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

type exportJob struct {
	obj       *governancev1alpha1.AgentDiagnostic
	onSuccess func()
	onFailure func(err error)
}

const (
	// jobQueueMultiplier is the factor by which concurrency is multiplied to set the job channel capacity.
	jobQueueMultiplier = 10
)

// ExportPool implements a bounded worker pool for exporting AgentDiagnostics.
type ExportPool struct {
	exporter Exporter
	jobs     chan exportJob
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// NewExportPool creates a pool with concurrency workers and a job queue of size concurrency*jobQueueMultiplier.
func NewExportPool(ctx context.Context, concurrency int, exporter Exporter) *ExportPool {
	p := &ExportPool{
		exporter: exporter,
		jobs:     make(chan exportJob, concurrency*jobQueueMultiplier),
	}

	for range concurrency {
		p.wg.Add(1)
		go p.worker(ctx)
	}

	return p
}

func (p *ExportPool) worker(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-p.jobs:
			if !ok {
				return
			}
			if err := p.exporter.Export(ctx, job.obj); err != nil {
				job.onFailure(err)
			} else {
				job.onSuccess()
			}
		}
	}
}

// Submit enqueues obj for export. Returns true if accepted, false if the
// pool is full (caller must skip and retry next cycle — never block here).
func (p *ExportPool) Submit(obj *governancev1alpha1.AgentDiagnostic, onSuccess func(), onFailure func(error)) bool {
	select {
	case p.jobs <- exportJob{obj: obj, onSuccess: onSuccess, onFailure: onFailure}:
		return true
	default:
		return false
	}
}

// Stop closes the jobs channel and waits for all in-flight workers to finish.
func (p *ExportPool) Stop() {
	p.stopOnce.Do(func() {
		close(p.jobs)
		p.wg.Wait()
	})
}
