package pattern

import (
	"bufio"
	"context"
	"os"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/slices"

	"github.com/grafana/dskit/ring"
	ring_client "github.com/grafana/dskit/ring/client"
	"github.com/grafana/dskit/services"

	"github.com/grafana/loki/v3/pkg/logproto"
	"github.com/grafana/loki/v3/pkg/pattern/metric"
)

func Test_prunePatterns(t *testing.T) {
	file, err := os.Open(`testdata/patterns.txt`)
	require.NoError(t, err)
	defer file.Close()

	resp := new(logproto.QueryPatternsResponse)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		resp.Series = append(resp.Series, logproto.NewPatternSeries(scanner.Text(), []*logproto.PatternSample{}))
	}
	require.NoError(t, scanner.Err())
	prunePatterns(resp, 0)

	expectedPatterns := []string{
		`<_> caller=aggregator.go:139 level=info msg="received kafka message" topic=cortex-dev-01-aggregations partition=<_>`,
		`<_> caller=batcher.go:155 level=info msg="batcher:processing aggregation result" <_> partitionID=<_> +0000 UTC, <_>`,
		`<_> caller=batcher.go:155 level=info msg="batcher:processing aggregation result" result="user=9960, partitionID=<_> +0000 UTC, <_>`,
		`<_> caller=batcher.go:155 level=info msg="batcher:processing aggregation result" result="user=9960, partitionID=<_> sampleTimestamp=2024-04-03 <_> +0000 UTC, <_>`,
		`<_> caller=offset_committer.go:174 level=info msg="partition offset committer committed offset" topic=cortex-dev-01-aggregations partition=<_> +0000 UTC" <_> +0000 UTC" <_> currentBuckets="unsupported value type"`,
		`<_> caller=offset_committer.go:174 level=info msg="partition offset committer committed offset" topic=cortex-dev-01-aggregations partition=<_> handledMessageTime="2024-04-03 <_> +0000 UTC" <_> +0000 UTC" <_> currentBuckets="unsupported value type"`,
		`<_> caller=wrapper.go:48 level=info component=distributor msg="sample remote write" eventType=bi <_>`,
	}

	patterns := make([]string, 0, len(resp.Series))
	for _, p := range resp.Series {
		patterns = append(patterns, p.GetPattern())
	}
	slices.Sort(patterns)

	require.Equal(t, expectedPatterns, patterns)
}

func Test_Samples(t *testing.T) {
	t.Run("it rejects metric queries with filters", func(t *testing.T) {
		q := &IngesterQuerier{
			cfg: Config{
				MetricAggregation: metric.AggregationConfig{
					Enabled: true,
				},
			},
			logger:     log.NewNopLogger(),
			ringClient: &fakeRingClient{},
			registerer: nil,
		}
		for _, query := range []string{
			`count_over_time({foo="bar"} |= "baz" [5m])`,
			`count_over_time({foo="bar"} != "baz" [5m])`,
			`count_over_time({foo="bar"} |~ "baz" [5m])`,
			`count_over_time({foo="bar"} !~ "baz" [5m])`,
			`count_over_time({foo="bar"} | logfmt | color="blue" [5m])`,
			`sum(count_over_time({foo="bar"} |= "baz" [5m]))`,
			`sum by (label)(count_over_time({foo="bar"} |= "baz" [5m]))`,
			`bytes_over_time({foo="bar"} |= "baz" [5m])`,
		} {
			_, err := q.Samples(
				context.Background(),
				&logproto.QuerySamplesRequest{
					Query: query,
				},
			)
			require.Error(t, err, query)
			require.ErrorIs(t, err, ErrParseQuery, query)
		}
	})

	t.Run("it rejects log selector queries", func(t *testing.T) {
		q := &IngesterQuerier{
			cfg: Config{
				MetricAggregation: metric.AggregationConfig{
					Enabled: true,
				},
			},
			logger:     log.NewNopLogger(),
			ringClient: &fakeRingClient{},
			registerer: nil,
		}
		for _, query := range []string{
			`{foo="bar"}`,
		} {
			_, err := q.Samples(
				context.Background(),
				&logproto.QuerySamplesRequest{
					Query: query,
				},
			)
			require.Error(t, err, query)
			require.Equal(t, "only sample expression supported", err.Error(), query)
		}
	})

	t.Run("accepts count and bytes metric queries", func(t *testing.T) {
		q := &IngesterQuerier{
			cfg: Config{
				MetricAggregation: metric.AggregationConfig{
					Enabled: true,
				},
			},
			logger:     log.NewNopLogger(),
			ringClient: &fakeRingClient{},
			registerer: nil,
		}
		for _, query := range []string{
			`count_over_time({foo="bar"}[5m])`,
			`bytes_over_time({foo="bar"}[5m])`,
			`sum(count_over_time({foo="bar"}[5m]))`,
			`sum(bytes_over_time({foo="bar"}[5m]))`,
			`sum by (level)(count_over_time({foo="bar"}[5m]))`,
			`sum by (level)(bytes_over_time({foo="bar"}[5m]))`,
		} {
			_, err := q.Samples(
				context.Background(),
				&logproto.QuerySamplesRequest{
					Query: query,
				},
			)
			require.NoError(t, err, query)
		}
	})
}

type fakeRingClient struct{}

func (f *fakeRingClient) Pool() *ring_client.Pool {
	panic("not implemented")
}

func (f *fakeRingClient) StartAsync(_ context.Context) error {
	panic("not implemented")
}

func (f *fakeRingClient) AwaitRunning(_ context.Context) error {
	panic("not implemented")
}

func (f *fakeRingClient) StopAsync() {
	panic("not implemented")
}

func (f *fakeRingClient) AwaitTerminated(_ context.Context) error {
	panic("not implemented")
}

func (f *fakeRingClient) FailureCase() error {
	panic("not implemented")
}

func (f *fakeRingClient) State() services.State {
	panic("not implemented")
}

func (f *fakeRingClient) AddListener(_ services.Listener) {
	panic("not implemented")
}

func (f *fakeRingClient) Ring() ring.ReadRing {
	return &fakeRing{}
}

type fakeRing struct{}

func (f *fakeRing) Get(
	_ uint32,
	_ ring.Operation,
	_ []ring.InstanceDesc,
	_ []string,
	_ []string,
) (ring.ReplicationSet, error) {
	panic("not implemented")
}

func (f *fakeRing) GetAllHealthy(_ ring.Operation) (ring.ReplicationSet, error) {
	panic("not implemented")
}

func (f *fakeRing) GetReplicationSetForOperation(_ ring.Operation) (ring.ReplicationSet, error) {
	return ring.ReplicationSet{}, nil
}

func (f *fakeRing) ReplicationFactor() int {
	panic("not implemented")
}

func (f *fakeRing) InstancesCount() int {
	panic("not implemented")
}

func (f *fakeRing) ShuffleShard(_ string, _ int) ring.ReadRing {
	panic("not implemented")
}

func (f *fakeRing) GetInstanceState(_ string) (ring.InstanceState, error) {
	panic("not implemented")
}

func (f *fakeRing) ShuffleShardWithLookback(
	_ string,
	_ int,
	_ time.Duration,
	_ time.Time,
) ring.ReadRing {
	panic("not implemented")
}

func (f *fakeRing) HasInstance(_ string) bool {
	panic("not implemented")
}

func (f *fakeRing) CleanupShuffleShardCache(_ string) {
	panic("not implemented")
}

func (f *fakeRing) GetTokenRangesForInstance(_ string) (ring.TokenRanges, error) {
	panic("not implemented")
}
