package pattern

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/grafana/loki/v3/pkg/logproto"
	"github.com/grafana/loki/v3/pkg/logql"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/grafana/loki/v3/pkg/logqlmodel"
	"github.com/grafana/loki/v3/pkg/pattern/drain"
	"github.com/grafana/loki/v3/pkg/pattern/iter"
	"github.com/grafana/loki/v3/pkg/pattern/metric"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
)

type stream struct {
	fp           model.Fingerprint
	labels       labels.Labels
	labelsString string
	labelHash    uint64
	patterns     *drain.Drain
	mtx          sync.Mutex

	aggregateMetrics bool
	metrics          *metric.Chunks

	evaluator metric.SampleEvaluatorFactory

	lastTs int64
}

func newStream(
	fp model.Fingerprint,
	labels labels.Labels,
	metrics *ingesterMetrics,
	aggregateMetrics bool,
) (*stream, error) {
	stream := &stream{
		fp:           fp,
		labels:       labels,
		labelsString: labels.String(),
		labelHash:    labels.Hash(),
		patterns: drain.New(drain.DefaultConfig(), &drain.Metrics{
			PatternsEvictedTotal:  metrics.patternsDiscardedTotal,
			PatternsDetectedTotal: metrics.patternsDetectedTotal,
		}),
		aggregateMetrics: aggregateMetrics,
	}

	if aggregateMetrics {
		chunks := metric.NewChunks(labels)
		stream.metrics = chunks
		stream.evaluator = metric.NewDefaultEvaluatorFactory(chunks)
	}

	return stream, nil
}

func (s *stream) Push(
	_ context.Context,
	entries []logproto.Entry,
) error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	bytes := uint64(0)
	count := uint64(len(entries))
	for _, entry := range entries {
		if entry.Timestamp.UnixNano() < s.lastTs {
			continue
		}

		bytes += uint64(len(entry.Line))

		s.lastTs = entry.Timestamp.UnixNano()
		s.patterns.Train(entry.Line, entry.Timestamp.UnixNano())
	}

	if s.aggregateMetrics && s.metrics != nil {
		s.metrics.Observe(bytes, count, model.TimeFromUnixNano(s.lastTs))
	}
	return nil
}

func (s *stream) Iterator(_ context.Context, from, through, step model.Time) (iter.Iterator, error) {
	// todo we should improve locking.
	s.mtx.Lock()
	defer s.mtx.Unlock()

	clusters := s.patterns.Clusters()
	iters := make([]iter.Iterator, 0, len(clusters))

	for _, cluster := range clusters {
		if cluster.String() == "" {
			continue
		}
		iters = append(iters, cluster.Iterator(from, through, step))
	}
	return iter.NewMerge(iters...), nil
}

func (s *stream) SampleIterator(
	ctx context.Context,
	expr syntax.SampleExpr,
	from, through, step model.Time,
) (iter.Iterator, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	stepEvaluator, err := s.evaluator.NewStepEvaluator(
		ctx,
		s.evaluator,
		expr,
		from,
		through,
		step,
	)
	if err != nil {
		return nil, err
	}

	next, ts, r := stepEvaluator.Next()
	if stepEvaluator.Error() != nil {
		return nil, stepEvaluator.Error()
	}

	// TODO(twhitney): actually get max series from limits
	// this is only 1 series since we're already on a stream
	// this this limit needs to also be enforced higher up
	maxSeries := 1000
	series, err := s.JoinSampleVector(
		next,
		ts,
		r,
		stepEvaluator,
		maxSeries,
		from, through, step)
	if err != nil {
		return nil, err
	}

	return metric.NewSeriesToSampleIterator(series), nil
}

//TODO: should this join multiple series into a matrix, so we don't have the weird hack?
func (s *stream) JoinSampleVector(
	next bool,
	ts int64,
	r logql.StepResult,
	stepEvaluator logql.StepEvaluator,
	maxSeries int,
	from, through, step model.Time,
) (*promql.Series, error) {
	stepCount := int(math.Ceil(float64(through.Sub(from).Nanoseconds()) / float64(step.UnixNano())))
	if stepCount <= 0 {
		stepCount = 1
	}

	vec := promql.Vector{}
	if next {
		vec = r.SampleVector()
	}

	// fail fast for the first step or instant query
	if len(vec) > maxSeries {
		return nil, logqlmodel.NewSeriesLimitError(maxSeries)
	}

	var seriesHash string
	series := map[string]*promql.Series{}
	for next {
		vec = r.SampleVector()
		for _, p := range vec {
			seriesHash = p.Metric.String()
			s, ok := series[seriesHash]
			if !ok {
				s = &promql.Series{
					Metric: p.Metric,
					Floats: make([]promql.FPoint, 0, stepCount),
				}
				series[p.Metric.String()] = s
			}

			s.Floats = append(s.Floats, promql.FPoint{
				T: ts,
				F: p.F,
			})
		}

		next, ts, r = stepEvaluator.Next()
		if stepEvaluator.Error() != nil {
			return nil, stepEvaluator.Error()
		}
	}

	if len(series) > 1 {
		// TODO: is this actually a problem? Should this just become a Matrix
		return nil, errors.New("multiple series found in a single stream")
	}

	return series[seriesHash], stepEvaluator.Error()
}

func (s *stream) prune(olderThan time.Duration) bool {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	clusters := s.patterns.Clusters()
	for _, cluster := range clusters {
		cluster.Prune(olderThan)
		if cluster.Size == 0 {
			s.patterns.Delete(cluster)
		}
	}

	return len(s.patterns.Clusters()) == 0
}
