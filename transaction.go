// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package apm // import "go.elastic.co/apm/v2"

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"math/rand"
	"sync"
	"time"
)

const (
	// maxDroppedSpanStats sets the hard limit for the number of dropped span
	// stats that are stored in a transaction.
	maxDroppedSpanStats = 128
)

// StartTransaction returns a new Transaction with the specified
// name and type, and with the start time set to the current time.
// This is equivalent to calling StartTransactionOptions with a
// zero TransactionOptions.
func (t *Tracer) StartTransaction(name, transactionType string) *Transaction {
	return t.StartTransactionOptions(name, transactionType, TransactionOptions{})
}

// StartTransactionOptions returns a new Transaction with the
// specified name, type, and options.
func (t *Tracer) StartTransactionOptions(name, transactionType string, opts TransactionOptions) *Transaction {
	td, _ := t.transactionDataPool.Get().(*TransactionData)
	if td == nil {
		td = &TransactionData{
			Duration: -1,
			Context: Context{
				captureBodyMask: CaptureBodyTransactions,
			},
			spanTimings:       make(spanTimingsMap),
			droppedSpansStats: make(droppedSpanTimingsMap, maxDroppedSpanStats),
		}
		var seed int64
		if err := binary.Read(cryptorand.Reader, binary.LittleEndian, &seed); err != nil {
			seed = time.Now().UnixNano()
		}
		td.rand = rand.New(rand.NewSource(seed))
	}
	tx := &Transaction{tracer: t, TransactionData: td}

	// Take a snapshot of config that should apply to all spans within the
	// transaction.
	instrumentationConfig := t.instrumentationConfig()
	tx.recording = instrumentationConfig.recording
	if !tx.recording || !t.Active() {
		return tx
	}

	tx.maxSpans = instrumentationConfig.maxSpans
	tx.compressedSpan.options = instrumentationConfig.compressionOptions
	tx.exitSpanMinDuration = instrumentationConfig.exitSpanMinDuration
	tx.spanStackTraceMinDuration = instrumentationConfig.spanStackTraceMinDuration
	tx.stackTraceLimit = instrumentationConfig.stackTraceLimit
	tx.Context.captureHeaders = instrumentationConfig.captureHeaders
	tx.propagateLegacyHeader = instrumentationConfig.propagateLegacyHeader
	tx.Context.sanitizedFieldNames = instrumentationConfig.sanitizedFieldNames
	tx.breakdownMetricsEnabled = t.breakdownMetrics.enabled

	continuationStrategy := instrumentationConfig.continuationStrategy
	shouldRestartTrace := false
	if continuationStrategy == "restart_external" {
		if opts.TraceContext.State.haveElastic {
			continuationStrategy = "continue"
		} else {
			continuationStrategy = "restart"
		}
	}

	if continuationStrategy == "restart" {
		if !opts.TraceContext.Trace.isZero() && !opts.TraceContext.Span.isZero() {
			link := SpanLink{
				Trace: opts.TraceContext.Trace,
				Span:  opts.TraceContext.Span,
			}
			tx.links = append(tx.links, link)
			shouldRestartTrace = true
		}
	}

	var root bool
	if opts.TraceContext.Trace.Validate() == nil && !shouldRestartTrace {
		tx.traceContext.Trace = opts.TraceContext.Trace
		tx.traceContext.Options = opts.TraceContext.Options
		if opts.TraceContext.Span.Validate() == nil {
			tx.parentID = opts.TraceContext.Span
		}
		if opts.TransactionID.Validate() == nil {
			tx.traceContext.Span = opts.TransactionID
		} else {
			binary.LittleEndian.PutUint64(tx.traceContext.Span[:], tx.rand.Uint64())
		}
		if opts.TraceContext.State.Validate() == nil {
			tx.traceContext.State = opts.TraceContext.State
		}
	} else {
		// Start a new trace. We reuse the trace ID for the root transaction's ID
		// if one is not specified in the options.
		root = true
		binary.LittleEndian.PutUint64(tx.traceContext.Trace[:8], tx.rand.Uint64())
		binary.LittleEndian.PutUint64(tx.traceContext.Trace[8:], tx.rand.Uint64())
		if opts.TransactionID.Validate() == nil {
			tx.traceContext.Span = opts.TransactionID
		} else {
			copy(tx.traceContext.Span[:], tx.traceContext.Trace[:])
		}
	}

	if root {
		var result SampleResult
		if instrumentationConfig.sampler != nil {
			result = instrumentationConfig.sampler.Sample(SampleParams{
				TraceContext: tx.traceContext,
			})
			if !result.Sampled {
				// Special case: for unsampled transactions we
				// report a sample rate of 0, so that we do not
				// count them in aggregations in the server.
				// This is necessary to avoid overcounting, as
				// we will scale the sampled transactions.
				result.SampleRate = 0
			}
			sampleRate := roundSampleRate(result.SampleRate)
			tx.traceContext.State = NewTraceState(TraceStateEntry{
				Key:   elasticTracestateVendorKey,
				Value: formatElasticTracestateValue(sampleRate),
			})
		} else {
			result.Sampled = true
		}
		if result.Sampled {
			o := tx.traceContext.Options.WithRecorded(true)
			tx.traceContext.Options = o
		}
	} else {
		// TODO(axw) make this behaviour configurable. In some cases
		// it may not be a good idea to honour the recorded flag, as
		// it may open up the application to DoS by forced sampling.
		// Even ignoring bad actors, a service that has many feeder
		// applications may end up being sampled at a very high rate.
		tx.traceContext.Options = opts.TraceContext.Options
	}

	tx.Name = name
	tx.Type = transactionType
	tx.timestamp = opts.Start
	if tx.timestamp.IsZero() {
		tx.timestamp = time.Now()
	}
	tx.links = append(tx.links, opts.Links...)
	return tx
}

// TransactionOptions holds options for Tracer.StartTransactionOptions.
type TransactionOptions struct {
	// TraceContext holds the TraceContext for a new transaction. If this is
	// zero, a new trace will be started.
	TraceContext TraceContext

	// TransactionID holds the ID to assign to the transaction. If this is
	// zero, a new ID will be generated and used instead.
	TransactionID SpanID

	// Start is the start time of the transaction. If this has the
	// zero value, time.Now() will be used instead.
	Start time.Time

	// Links, if non-nil, holds a list of spans linked to the transaction.
	Links []SpanLink
}

// Transaction describes an event occurring in the monitored service.
type Transaction struct {
	tracer       *Tracer
	traceContext TraceContext
	parentID     SpanID

	mu sync.RWMutex

	// TransactionData holds the transaction data. This field is set to
	// nil when either of the transaction's End or Discard methods are called.
	*TransactionData
}

// Sampled reports whether or not the transaction is sampled.
func (tx *Transaction) Sampled() bool {
	if tx == nil {
		return false
	}
	return tx.traceContext.Options.Recorded()
}

// TraceContext returns the transaction's TraceContext.
//
// The resulting TraceContext's Span field holds the transaction's ID.
// If tx is nil, a zero (invalid) TraceContext is returned.
func (tx *Transaction) TraceContext() TraceContext {
	if tx == nil {
		return TraceContext{}
	}
	return tx.traceContext
}

// ShouldPropagateLegacyHeader reports whether instrumentation should
// propagate the legacy "Elastic-Apm-Traceparent" header in addition to
// the standard W3C "traceparent" header.
//
// This method will be removed in a future major version when we remove
// support for propagating the legacy header.
func (tx *Transaction) ShouldPropagateLegacyHeader() bool {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.ended() {
		return false
	}
	return tx.propagateLegacyHeader
}

// EnsureParent returns the span ID for for tx's parent, generating a
// parent span ID if one has not already been set and tx has not been
// ended. If tx is nil or has been ended, a zero (invalid) SpanID is
// returned.
//
// This method can be used for generating a span ID for the RUM
// (Real User Monitoring) agent, where the RUM agent is initialized
// after the backend service returns.
func (tx *Transaction) EnsureParent() SpanID {
	if tx == nil {
		return SpanID{}
	}

	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.ended() {
		return SpanID{}
	}

	if tx.parentID.isZero() {
		// parentID can only be zero if tx is a root transaction
		// for which GenerateParentTraceContext() has not previously
		// been called. Reuse the latter half of the trace ID for
		// the parent span ID; the first half is used for the
		// transaction ID.
		copy(tx.parentID[:], tx.traceContext.Trace[8:])
	}
	return tx.parentID
}

// ParentID returns the ID of the transaction's Parent or a zero (invalid) SpanID.
func (tx *Transaction) ParentID() SpanID {
	if tx == nil {
		return SpanID{}
	}
	tx.mu.RLock()
	defer tx.mu.RUnlock()
	return tx.parentID
}

// Discard discards a previously started transaction.
//
// Calling Discard will set tx's TransactionData field to nil, so callers must
// ensure tx is not updated after Discard returns.
func (tx *Transaction) Discard() {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.ended() {
		return
	}
	tx.reset(tx.tracer)
	tx.TransactionData = nil
}

// End enqueues tx for sending to the Elastic APM server.
//
// Calling End will set tx's TransactionData field to nil, so callers
// must ensure tx is not updated after End returns.
//
// If tx.Duration has not been set, End will set it to the elapsed time
// since the transaction's start time.
func (tx *Transaction) End() {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.ended() {
		return
	}
	if tx.Type == "" {
		tx.Type = "custom"
	}
	if tx.recording {
		if tx.Duration < 0 {
			tx.Duration = time.Since(tx.timestamp)
		}
		if tx.Outcome == "" {
			tx.Outcome = tx.Context.outcome()
			if tx.Outcome == "" {
				if tx.errorCaptured {
					tx.Outcome = "failure"
				} else {
					tx.Outcome = "success"
				}
			}
		}
		// Hold the transaction data lock to check if the transaction has any
		// compressed spans in its cache, if so, evict cache and end the span.
		tx.TransactionData.mu.Lock()
		if evictedSpan := tx.compressedSpan.evict(); evictedSpan != nil {
			evictedSpan.end()
		}
		tx.TransactionData.mu.Unlock()
		tx.enqueue()
	} else {
		tx.reset(tx.tracer)
	}
	tx.TransactionData = nil
}

func (tx *Transaction) enqueue() {
	event := tracerEvent{eventType: transactionEvent}
	event.tx.Transaction = tx
	event.tx.TransactionData = tx.TransactionData
	select {
	case tx.tracer.events <- event:
	default:
		// Enqueuing a transaction should never block.
		tx.tracer.breakdownMetrics.recordTransaction(tx.TransactionData)

		tx.tracer.stats.accumulate(TracerStats{TransactionsDropped: 1})
		tx.reset(tx.tracer)
	}
}

// ended reports whether or not End or Discard has been called.
//
// This must be called with tx.mu held.
func (tx *Transaction) ended() bool {
	return tx.TransactionData == nil
}

// TransactionData holds the details for a transaction, and is embedded
// inside Transaction. When a transaction is ended, its TransactionData
// field will be set to nil.
type TransactionData struct {
	// Name holds the transaction name, initialized with the value
	// passed to StartTransaction.
	Name string

	// Type holds the transaction type, initialized with the value
	// passed to StartTransaction.
	Type string

	// Duration holds the transaction duration, initialized to -1.
	//
	// If you do not update Duration, calling Transaction.End will
	// calculate the duration based on the elapsed time since the
	// transaction's start time.
	Duration time.Duration

	// Context describes the context in which the transaction occurs.
	Context Context

	// Result holds the transaction result.
	Result string

	// Outcome holds the transaction outcome: success, failure, or
	// unknown (the default). If Outcome is set to something else,
	// it will be replaced with "unknown".
	//
	// Outcome is used for error rate calculations. A value of "success"
	// indicates that a transaction succeeded, while "failure" indicates
	// that the transaction failed. If Outcome is set to "unknown" (or
	// some other value), then the transaction will not be included in
	// error rate calculations.
	Outcome string

	recording                 bool
	maxSpans                  int
	exitSpanMinDuration       time.Duration
	spanStackTraceMinDuration time.Duration
	stackTraceLimit           int
	breakdownMetricsEnabled   bool
	propagateLegacyHeader     bool
	timestamp                 time.Time

	links             []SpanLink
	mu                sync.Mutex
	errorCaptured     bool
	spansCreated      int
	spansDropped      int
	childrenTimer     childrenTimer
	spanTimings       spanTimingsMap
	droppedSpansStats droppedSpanTimingsMap
	rand              *rand.Rand // for ID generation

	compressedSpan compressedSpan
}

// reset resets the TransactionData back to its zero state and places it back
// into the transaction pool.
func (td *TransactionData) reset(tracer *Tracer) {
	*td = TransactionData{
		Context:           td.Context,
		Duration:          -1,
		rand:              td.rand,
		spanTimings:       td.spanTimings,
		droppedSpansStats: td.droppedSpansStats,
	}
	td.Context.reset()
	td.spanTimings.reset()
	td.droppedSpansStats.reset()
	tracer.transactionDataPool.Put(td)
}

type droppedSpanTimingsKey struct {
	destination string
	outcome     string
}

// droppedSpanTimingsMap records span timings for groups of dropped spans.
type droppedSpanTimingsMap map[droppedSpanTimingsKey]spanTiming

// add accumulates the timing for a {destination, outcome} pair, silently drops
// any pairs that would cause the map to exceed the maxDroppedSpanStats.
func (m droppedSpanTimingsMap) add(dst, outcome string, count int, d time.Duration) {
	k := droppedSpanTimingsKey{destination: dst, outcome: outcome}
	timing, ok := m[k]
	if ok || maxDroppedSpanStats > len(m) {
		timing.count += uint64(count)
		timing.duration += int64(d)
		m[k] = timing
	}
}

// reset resets m back to its initial zero state.
func (m droppedSpanTimingsMap) reset() {
	for k := range m {
		delete(m, k)
	}
}
