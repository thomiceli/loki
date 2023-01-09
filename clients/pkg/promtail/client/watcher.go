package client

import (
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/tsdb/wlog"

	"github.com/grafana/loki/pkg/ingester"
)

const (
	readPeriod         = 10 * time.Millisecond
	checkpointPeriod   = 5 * time.Second
	segmentCheckPeriod = 100 * time.Millisecond
)

// Based in the implementation of prometheus WAL watcher
// https://github.com/prometheus/prometheus/blob/main/tsdb/wlog/watcher.go. Includes some changes to make it suitable
// for log WAL entries, but also, the consumer surface has been implemented according to the actions necessary for
// Promtail's WAL.

// WALReader is a dependency interface to inject generic WAL readers into the WALWatcher.
type WALReader interface {
	Next() bool
	Err() error
	Record() []byte
}

// WatcherConsumer is responsible for doing the necessary work to process both series and entries while the WALWatcher
// is reading / tailing segments. Also, when a new segment is detected, and the watcher is moving on from one to the other,
// the SegmentEnd callback is available to take action if required.
// The implementor of this interface is not expected to implement thread safety if used on a single watcher, since each
// callback is called synchronously.
type WatcherConsumer interface {
	ConsumeSeries(series record.RefSeries) error
	ConsumeEntries(entries ingester.RefEntries) error
	SegmentEnd(segmentNum int)
}

type WALWatcher struct {
	name       string
	consumer   WatcherConsumer
	done       chan struct{}
	quit       chan struct{}
	walDir     string
	logger     log.Logger
	MaxSegment int

	metrics *WALWatcherMetrics
}

func NewWALWatcher(walDir, name string, metrics *WALWatcherMetrics, consumer WatcherConsumer, logger log.Logger) *WALWatcher {
	return &WALWatcher{
		walDir:     walDir,
		name:       name,
		consumer:   consumer,
		quit:       make(chan struct{}),
		done:       make(chan struct{}),
		MaxSegment: -1,
		logger:     logger,
		metrics:    metrics,
	}
}

// Start runs the watcher main loop.
func (w *WALWatcher) Start() {
	w.metrics.watchersRunning.WithLabelValues().Inc()
	go w.mainLoop()
}

// mainLoop retries when there's an error reading a specific segment or advancing one, but leaving a bigger time in-between
// retries.
func (w *WALWatcher) mainLoop() {
	defer close(w.done)
	for !isClosed(w.quit) {
		if err := w.run(); err != nil {
			level.Error(w.logger).Log("msg", "error tailing WAL", "err", err)
		}

		select {
		case <-w.quit:
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// Run the watcher, which will tail the WAL until the quit channel is closed
// or an error case is hit.
func (w *WALWatcher) run() error {
	_, lastSegment, err := w.firstAndLast()
	if err != nil {
		return fmt.Errorf("wal.Segments: %w", err)
	}

	currentSegment := lastSegment
	level.Debug(w.logger).Log("msg", "Tailing WAL", "currentSegment", currentSegment, "lastSegment", lastSegment)
	for !isClosed(w.quit) {
		w.metrics.currentSegment.WithLabelValues(w.name).Set(float64(currentSegment))
		level.Debug(w.logger).Log("msg", "Processing segment", "currentSegment", currentSegment)

		// On start, we have a pointer to what is the latest segment. On subsequent calls to this function,
		// currentSegment will have been incremented, and we should open that segment.
		if err := w.watch(currentSegment, currentSegment >= lastSegment); err != nil {
			return err
		}

		// For testing: stop when you hit a specific segment.
		if currentSegment == w.MaxSegment {
			return nil
		}

		// We know new segment has been cut upon advancing the segment pointer. Call the consumer's `SegmentEnd`
		// callback to notify that a segment was read fully.
		w.consumer.SegmentEnd(currentSegment)
		currentSegment++
	}

	return nil
}

// Use tail true to indicate that the reader is currently on a segment that is
// actively being written to. If false, assume it's a full segment, and we're
// replaying it.
func (w *WALWatcher) watch(segmentNum int, tail bool) error {
	segment, err := wlog.OpenReadSegment(wlog.SegmentName(w.walDir, segmentNum))
	if err != nil {
		return err
	}
	defer segment.Close()

	reader := wlog.NewLiveReader(w.logger, nil, segment)

	readTicker := time.NewTicker(readPeriod)
	defer readTicker.Stop()

	checkpointTicker := time.NewTicker(checkpointPeriod)
	defer checkpointTicker.Stop()

	segmentTicker := time.NewTicker(segmentCheckPeriod)
	defer segmentTicker.Stop()

	// If we're replaying the segment we need to know the size of the file to know
	// when to return from watch and move on to the next segment.
	size := int64(math.MaxInt64)
	if !tail {
		segmentTicker.Stop()
		checkpointTicker.Stop()
		var err error
		size, err = getSegmentSize(w.walDir, segmentNum)
		if err != nil {
			return fmt.Errorf("getSegmentSize: %w", err)
		}
	}

	for {
		select {
		case <-w.quit:
			return nil

		case <-segmentTicker.C:
			_, last, err := w.firstAndLast()
			if err != nil {
				return fmt.Errorf("segments: %w", err)
			}

			// Check if new segments exists.
			if last <= segmentNum {
				continue
			}

			// Since we know last > segmentNum, there must be a new segment. Read the remaining from the segmentNum segment
			// and return from `watch` to read the next one
			err = w.readSegment(reader, segmentNum)

			// Ignore errors reading to end of segment whilst replaying the WAL.
			if !tail {
				w.logIgnoredErrorWhileReplaying(err, reader.Offset(), size, segmentNum)
				return nil
			}

			// Otherwise, when we are tailing, non-EOFs are fatal.
			if errors.Cause(err) != io.EOF {
				return err
			}

			return nil

		case <-readTicker.C:
			err = w.readSegment(reader, segmentNum)

			// Ignore all errors reading to end of segment whilst replaying the WAL.
			if !tail {
				w.logIgnoredErrorWhileReplaying(err, reader.Offset(), size, segmentNum)
				return nil
			}

			// Otherwise, when we are tailing, non-EOFs are fatal.
			if errors.Cause(err) != io.EOF {
				return err
			}
		}
	}
}

func (w *WALWatcher) logIgnoredErrorWhileReplaying(err error, readerOffset, size int64, segmentNum int) {
	if err != nil && errors.Cause(err) != io.EOF {
		level.Warn(w.logger).Log("msg", "Ignoring error reading to end of segment, may have dropped data", "segment", segmentNum, "err", err)
	} else if readerOffset != size {
		level.Warn(w.logger).Log("msg", "Expected to have read whole segment, may have dropped data", "segment", segmentNum, "read", readerOffset, "size", size)
	}
}

// Read entries from a segment, decode them and dispatch them.
func (w *WALWatcher) readSegment(r *wlog.LiveReader, segmentNum int) error {
	for r.Next() && !isClosed(w.quit) {
		b := r.Record()
		if err := r.Err(); err != nil {
			return err
		}
		w.metrics.recordsRead.WithLabelValues(w.name).Inc()

		if err := w.decodeAndDispatch(b); err != nil {
			return err
		}
	}
	return errors.Wrapf(r.Err(), "segment %d: %v", segmentNum, r.Err())
}

// decodeAndDispatch first decodes a WAL record. Upon reading either Series or Entries from the WAL record, call the
// appropriate callbacks in the consumer.
func (w *WALWatcher) decodeAndDispatch(b []byte) error {
	rec := recordPool.GetRecord()
	if err := ingester.DecodeWALRecord(b, rec); err != nil {
		w.metrics.recordDecodeFails.WithLabelValues(w.name).Inc()
		return err
	}

	// First process all series to ensure we don't write entries to non-existent series.
	var firstErr error
	for _, s := range rec.Series {
		if err := w.consumer.ConsumeSeries(s); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	for _, entries := range rec.RefEntries {
		if err := w.consumer.ConsumeEntries(entries); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

func (w *WALWatcher) Stop() {
	// first close the quit channel to order main mainLoop routine to stop
	close(w.quit)
	// upon calling stop, wait for main mainLoop execution to stop
	<-w.done

	w.metrics.watchersRunning.WithLabelValues().Dec()
}

// firstAndList finds the first and last segment number for a WAL directory.
func (w *WALWatcher) firstAndLast() (int, int, error) {
	refs, err := w.segments(w.walDir)
	if err != nil {
		return -1, -1, err
	}

	if len(refs) == 0 {
		return -1, -1, nil
	}
	return refs[0], refs[len(refs)-1], nil
}

// Copied from tsdb/wlog/wlog.go so we do not have to open a WAL.
// Plan is to move WAL watcher to TSDB and dedupe these implementations.
func (w *WALWatcher) segments(dir string) ([]int, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var refs []int
	for _, f := range files {
		k, err := strconv.Atoi(f.Name())
		if err != nil {
			continue
		}
		refs = append(refs, k)
	}
	sort.Ints(refs)
	for i := 0; i < len(refs)-1; i++ {
		if refs[i]+1 != refs[i+1] {
			return nil, fmt.Errorf("segments are not sequential")
		}
	}
	return refs, nil
}

// isClosed checks in a non-blocking manner if a channel is closed or not.
func isClosed(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// Get size of segment.
func getSegmentSize(dir string, index int) (int64, error) {
	i := int64(-1)
	fi, err := os.Stat(wlog.SegmentName(dir, index))
	if err == nil {
		i = fi.Size()
	}
	return i, err
}
