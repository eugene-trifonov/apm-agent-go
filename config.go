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
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/pkg/errors"

	"go.elastic.co/apm/v2/internal/apmlog"
	"go.elastic.co/apm/v2/internal/configutil"
	"go.elastic.co/apm/v2/internal/wildcard"
	"go.elastic.co/apm/v2/transport"
)

const (
	envMetricsInterval             = "ELASTIC_APM_METRICS_INTERVAL"
	envMaxSpans                    = "ELASTIC_APM_TRANSACTION_MAX_SPANS"
	envTransactionSampleRate       = "ELASTIC_APM_TRANSACTION_SAMPLE_RATE"
	envSanitizeFieldNames          = "ELASTIC_APM_SANITIZE_FIELD_NAMES"
	envCaptureHeaders              = "ELASTIC_APM_CAPTURE_HEADERS"
	envCaptureBody                 = "ELASTIC_APM_CAPTURE_BODY"
	envServiceName                 = "ELASTIC_APM_SERVICE_NAME"
	envServiceVersion              = "ELASTIC_APM_SERVICE_VERSION"
	envEnvironment                 = "ELASTIC_APM_ENVIRONMENT"
	envSpanStackTraceMinDuration   = "ELASTIC_APM_SPAN_STACK_TRACE_MIN_DURATION"
	envActive                      = "ELASTIC_APM_ACTIVE"
	envRecording                   = "ELASTIC_APM_RECORDING"
	envAPIRequestSize              = "ELASTIC_APM_API_REQUEST_SIZE"
	envAPIRequestTime              = "ELASTIC_APM_API_REQUEST_TIME"
	envAPIBufferSize               = "ELASTIC_APM_API_BUFFER_SIZE"
	envMetricsBufferSize           = "ELASTIC_APM_METRICS_BUFFER_SIZE"
	envDisableMetrics              = "ELASTIC_APM_DISABLE_METRICS"
	envIgnoreURLs                  = "ELASTIC_APM_TRANSACTION_IGNORE_URLS"
	deprecatedEnvIgnoreURLs        = "ELASTIC_APM_IGNORE_URLS"
	envGlobalLabels                = "ELASTIC_APM_GLOBAL_LABELS"
	envStackTraceLimit             = "ELASTIC_APM_STACK_TRACE_LIMIT"
	envCentralConfig               = "ELASTIC_APM_CENTRAL_CONFIG"
	envBreakdownMetrics            = "ELASTIC_APM_BREAKDOWN_METRICS"
	envUseElasticTraceparentHeader = "ELASTIC_APM_USE_ELASTIC_TRACEPARENT_HEADER"
	envCloudProvider               = "ELASTIC_APM_CLOUD_PROVIDER"
	envContinuationStrategy        = "ELASTIC_APM_TRACE_CONTINUATION_STRATEGY"

	// span_compression (default `true`)
	envSpanCompressionEnabled = "ELASTIC_APM_SPAN_COMPRESSION_ENABLED"
	// span_compression_exact_match_max_duration (default `50ms`)
	envSpanCompressionExactMatchMaxDuration = "ELASTIC_APM_SPAN_COMPRESSION_EXACT_MATCH_MAX_DURATION"
	// span_compression_same_kind_max_duration (default `0ms`)
	envSpanCompressionSameKindMaxDuration = "ELASTIC_APM_SPAN_COMPRESSION_SAME_KIND_MAX_DURATION"

	// exit_span_min_duration (default `1ms`)
	envExitSpanMinDuration = "ELASTIC_APM_EXIT_SPAN_MIN_DURATION"

	// NOTE(axw) profiling environment variables are experimental.
	// They may be removed in a future minor version without being
	// considered a breaking change.
	envCPUProfileInterval  = "ELASTIC_APM_CPU_PROFILE_INTERVAL"
	envCPUProfileDuration  = "ELASTIC_APM_CPU_PROFILE_DURATION"
	envHeapProfileInterval = "ELASTIC_APM_HEAP_PROFILE_INTERVAL"

	defaultAPIRequestSize            = 750 * configutil.KByte
	defaultAPIRequestTime            = 10 * time.Second
	defaultAPIBufferSize             = 1 * configutil.MByte
	defaultMetricsBufferSize         = 750 * configutil.KByte
	defaultMetricsInterval           = 30 * time.Second
	defaultMaxSpans                  = 500
	defaultCaptureHeaders            = true
	defaultCaptureBody               = CaptureBodyOff
	defaultSpanStackTraceMinDuration = 5 * time.Millisecond
	defaultStackTraceLimit           = 50
	defaultContinuationStrategy      = "continue"

	defaultExitSpanMinDuration = time.Millisecond

	minAPIBufferSize     = 10 * configutil.KByte
	maxAPIBufferSize     = 100 * configutil.MByte
	minAPIRequestSize    = 1 * configutil.KByte
	maxAPIRequestSize    = 5 * configutil.MByte
	minMetricsBufferSize = 10 * configutil.KByte
	maxMetricsBufferSize = 100 * configutil.MByte

	// Span Compressions default setting values
	defaultSpanCompressionEnabled               = true
	defaultSpanCompressionExactMatchMaxDuration = 50 * time.Millisecond
	defaultSpanCompressionSameKindMaxDuration   = 0
)

var (
	defaultSanitizedFieldNames = configutil.ParseWildcardPatterns(strings.Join([]string{
		"password",
		"passwd",
		"pwd",
		"secret",
		"*key",
		"*token*",
		"*session*",
		"*credit*",
		"*card*",
		"*auth*",
		"set-cookie",
	}, ","))
)

// Regular expression matching comment characters to escape in the User-Agent header value.
//
// See https://httpwg.org/specs/rfc7230.html#field.components
var httpComment = regexp.MustCompile("[^\\t \\x21-\\x27\\x2a-\\x5b\\x5d-\\x7e\\x80-\\xff]")

func initialTransport(serviceName, serviceVersion string) (transport.Transport, error) {
	// User-Agent should be "apm-agent-go/<agent-version> (service-name service-version)".
	service := serviceName
	if serviceVersion != "" {
		service += " " + httpComment.ReplaceAllString(serviceVersion, "_")
	}
	userAgent := fmt.Sprintf("%s (%s)", transport.DefaultUserAgent(), service)
	httpTransport, err := transport.NewHTTPTransport(transport.HTTPTransportOptions{
		UserAgent: userAgent,
	})
	if err != nil {
		return nil, err
	}
	return httpTransport, nil
}

func initialRequestDuration() (time.Duration, error) {
	return configutil.ParseDurationEnv(envAPIRequestTime, defaultAPIRequestTime)
}

func initialMetricsInterval() (time.Duration, error) {
	return configutil.ParseDurationEnv(envMetricsInterval, defaultMetricsInterval)
}

func initialMetricsBufferSize() (int, error) {
	size, err := configutil.ParseSizeEnv(envMetricsBufferSize, defaultMetricsBufferSize)
	if err != nil {
		return 0, err
	}
	if size < minMetricsBufferSize || size > maxMetricsBufferSize {
		return 0, errors.Errorf(
			"%s must be at least %s and less than %s, got %s",
			envMetricsBufferSize, minMetricsBufferSize, maxMetricsBufferSize, size,
		)
	}
	return int(size), nil
}

func initialAPIBufferSize() (int, error) {
	size, err := configutil.ParseSizeEnv(envAPIBufferSize, defaultAPIBufferSize)
	if err != nil {
		return 0, err
	}
	if size < minAPIBufferSize || size > maxAPIBufferSize {
		return 0, errors.Errorf(
			"%s must be at least %s and less than %s, got %s",
			envAPIBufferSize, minAPIBufferSize, maxAPIBufferSize, size,
		)
	}
	return int(size), nil
}

func initialAPIRequestSize() (int, error) {
	size, err := configutil.ParseSizeEnv(envAPIRequestSize, defaultAPIRequestSize)
	if err != nil {
		return 0, err
	}
	if size < minAPIRequestSize || size > maxAPIRequestSize {
		return 0, errors.Errorf(
			"%s must be at least %s and less than %s, got %s",
			envAPIRequestSize, minAPIRequestSize, maxAPIRequestSize, size,
		)
	}
	return int(size), nil
}

func initialMaxSpans() (int, error) {
	value := os.Getenv(envMaxSpans)
	if value == "" {
		return defaultMaxSpans, nil
	}
	max, err := strconv.Atoi(value)
	if err != nil {
		return 0, errors.Wrapf(err, "failed to parse %s", envMaxSpans)
	}
	return max, nil
}

// initialSampler returns a nil Sampler if all transactions should be sampled.
func initialSampler() (Sampler, error) {
	value := os.Getenv(envTransactionSampleRate)
	return parseSampleRate(envTransactionSampleRate, value)
}

// parseSampleRate parses a numeric sampling rate in the range [0,1.0], returning a Sampler.
func parseSampleRate(name, value string) (Sampler, error) {
	if value == "" {
		value = "1"
	}
	ratio, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse %s", name)
	}
	if ratio < 0.0 || ratio > 1.0 {
		return nil, errors.Errorf(
			"invalid value for %s: %s (out of range [0,1.0])",
			name, value,
		)
	}
	return NewRatioSampler(ratio), nil
}

func initialSanitizedFieldNames() wildcard.Matchers {
	return configutil.ParseWildcardPatternsEnv(envSanitizeFieldNames, defaultSanitizedFieldNames)
}

func initContinuationStrategy() (string, error) {
	value := os.Getenv(envContinuationStrategy)
	if value == "" {
		return defaultContinuationStrategy, nil
	}
	return value, validateContinuationStrategy(value)
}

func validateContinuationStrategy(value string) error {
	switch value {
	case "continue", "restart", "restart_external":
		return nil
	default:
		return fmt.Errorf("unknown continuation strategy: %s", value)
	}
}

func initialCaptureHeaders() (bool, error) {
	return configutil.ParseBoolEnv(envCaptureHeaders, defaultCaptureHeaders)
}

func initialCaptureBody() (CaptureBodyMode, error) {
	value := os.Getenv(envCaptureBody)
	if value == "" {
		return defaultCaptureBody, nil
	}
	return parseCaptureBody(envCaptureBody, value)
}

func parseCaptureBody(name, value string) (CaptureBodyMode, error) {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "all":
		return CaptureBodyAll, nil
	case "errors":
		return CaptureBodyErrors, nil
	case "transactions":
		return CaptureBodyTransactions, nil
	case "off":
		return CaptureBodyOff, nil
	}
	return -1, errors.Errorf("invalid %s value %q", name, value)
}

func initialService() (name, version, environment string) {
	name = os.Getenv(envServiceName)
	version = os.Getenv(envServiceVersion)
	environment = os.Getenv(envEnvironment)
	if name == "" {
		name = filepath.Base(os.Args[0])
		if runtime.GOOS == "windows" {
			name = strings.TrimSuffix(name, filepath.Ext(name))
		}
	}
	name = sanitizeServiceName(name)
	return name, version, environment
}

func initialSpanStackTraceMinDuration() (time.Duration, error) {
	return configutil.ParseDurationEnv(envSpanStackTraceMinDuration, defaultSpanStackTraceMinDuration)
}

func initialActive() (bool, error) {
	return configutil.ParseBoolEnv(envActive, true)
}

func initialRecording() (bool, error) {
	return configutil.ParseBoolEnv(envRecording, true)
}

func initialDisabledMetrics() wildcard.Matchers {
	return configutil.ParseWildcardPatternsEnv(envDisableMetrics, nil)
}

func initialIgnoreTransactionURLs() wildcard.Matchers {
	matchers := configutil.ParseWildcardPatternsEnv(envIgnoreURLs, nil)
	if len(matchers) == 0 {
		matchers = configutil.ParseWildcardPatternsEnv(deprecatedEnvIgnoreURLs, nil)
	}
	return matchers
}

func initialStackTraceLimit() (int, error) {
	value := os.Getenv(envStackTraceLimit)
	if value == "" {
		return defaultStackTraceLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil {
		return 0, errors.Wrapf(err, "failed to parse %s", envStackTraceLimit)
	}
	return limit, nil
}

func initialCentralConfigEnabled() (bool, error) {
	return configutil.ParseBoolEnv(envCentralConfig, true)
}

func initialBreakdownMetricsEnabled() (bool, error) {
	return configutil.ParseBoolEnv(envBreakdownMetrics, true)
}

func initialUseElasticTraceparentHeader() (bool, error) {
	return configutil.ParseBoolEnv(envUseElasticTraceparentHeader, true)
}

func initialSpanCompressionEnabled() (bool, error) {
	return configutil.ParseBoolEnv(envSpanCompressionEnabled,
		defaultSpanCompressionEnabled,
	)
}

func initialSpanCompressionExactMatchMaxDuration() (time.Duration, error) {
	return configutil.ParseDurationEnv(
		envSpanCompressionExactMatchMaxDuration,
		defaultSpanCompressionExactMatchMaxDuration,
	)
}

func initialSpanCompressionSameKindMaxDuration() (time.Duration, error) {
	return configutil.ParseDurationEnv(
		envSpanCompressionSameKindMaxDuration,
		defaultSpanCompressionSameKindMaxDuration,
	)
}

func initialCPUProfileIntervalDuration() (time.Duration, time.Duration, error) {
	interval, err := configutil.ParseDurationEnv(envCPUProfileInterval, 0)
	if err != nil || interval <= 0 {
		return 0, 0, err
	}
	duration, err := configutil.ParseDurationEnv(envCPUProfileDuration, 0)
	if err != nil || duration <= 0 {
		return 0, 0, err
	}
	return interval, duration, nil
}

func initialHeapProfileInterval() (time.Duration, error) {
	return configutil.ParseDurationEnv(envHeapProfileInterval, 0)
}

func initialExitSpanMinDuration() (time.Duration, error) {
	return configutil.ParseDurationEnvOptions(
		envExitSpanMinDuration, defaultExitSpanMinDuration,
		configutil.DurationOptions{MinimumDurationUnit: time.Microsecond},
	)
}

// updateRemoteConfig updates t and cfg with changes held in "attrs", and reverts to local
// config for config attributes that have been removed (exist in old but not in attrs).
//
// On return from updateRemoteConfig, unapplied config will have been removed from attrs.
func (t *Tracer) updateRemoteConfig(logger Logger, old, attrs map[string]string) {
	warningf := func(string, ...interface{}) {}
	debugf := func(string, ...interface{}) {}
	errorf := func(string, ...interface{}) {}
	if logger != nil {
		warningf = logger.Warningf
		debugf = logger.Debugf
		errorf = logger.Errorf
	}
	envName := func(k string) string {
		return "ELASTIC_APM_" + strings.ToUpper(k)
	}

	var updates []func(cfg *instrumentationConfig)
	for k, v := range attrs {
		if oldv, ok := old[k]; ok && oldv == v {
			continue
		}
		switch envName(k) {
		case envCaptureBody:
			value, err := parseCaptureBody(k, v)
			if err != nil {
				errorf("central config failure: %s", err)
				delete(attrs, k)
				continue
			} else {
				updates = append(updates, func(cfg *instrumentationConfig) {
					cfg.captureBody = value
				})
			}
		case envMaxSpans:
			value, err := strconv.Atoi(v)
			if err != nil {
				errorf("central config failure: failed to parse %s: %s", k, err)
				delete(attrs, k)
				continue
			} else {
				updates = append(updates, func(cfg *instrumentationConfig) {
					cfg.maxSpans = value
				})
			}
		case envExitSpanMinDuration:
			duration, err := configutil.ParseDurationOptions(v, configutil.DurationOptions{
				MinimumDurationUnit: time.Microsecond,
			})
			if err != nil {
				errorf("central config failure: failed to parse %s: %s", k, err)
				delete(attrs, k)
				continue
			}
			updates = append(updates, func(cfg *instrumentationConfig) {
				cfg.exitSpanMinDuration = duration
			})
		case envIgnoreURLs:
			matchers := configutil.ParseWildcardPatterns(v)
			updates = append(updates, func(cfg *instrumentationConfig) {
				cfg.ignoreTransactionURLs = matchers
			})
		case envRecording:
			recording, err := strconv.ParseBool(v)
			if err != nil {
				errorf("central config failure: failed to parse %s: %s", k, err)
				delete(attrs, k)
				continue
			} else {
				updates = append(updates, func(cfg *instrumentationConfig) {
					cfg.recording = recording
				})
			}
		case envSanitizeFieldNames:
			matchers := configutil.ParseWildcardPatterns(v)
			updates = append(updates, func(cfg *instrumentationConfig) {
				cfg.sanitizedFieldNames = matchers
			})
		case envContinuationStrategy:
			if err := validateContinuationStrategy(v); err != nil {
				errorf("central config failure: failed to parse %s: %s", k, err)
				delete(attrs, k)
				continue
			}
			updates = append(updates, func(cfg *instrumentationConfig) {
				cfg.continuationStrategy = v
			})
		case envSpanStackTraceMinDuration:
			duration, err := configutil.ParseDuration(v)
			if err != nil {
				errorf("central config failure: failed to parse %s: %s", k, err)
				delete(attrs, k)
				continue
			} else {
				updates = append(updates, func(cfg *instrumentationConfig) {
					cfg.spanStackTraceMinDuration = duration
				})
			}
		case envStackTraceLimit:
			limit, err := strconv.Atoi(v)
			if err != nil {
				errorf("central config failure: failed to parse %s: %s", k, err)
				delete(attrs, k)
				continue
			} else {
				updates = append(updates, func(cfg *instrumentationConfig) {
					cfg.stackTraceLimit = limit
				})
			}
		case envTransactionSampleRate:
			sampler, err := parseSampleRate(k, v)
			if err != nil {
				errorf("central config failure: %s", err)
				delete(attrs, k)
				continue
			} else {
				updates = append(updates, func(cfg *instrumentationConfig) {
					cfg.sampler = sampler
				})
			}
		case apmlog.EnvLogLevel:
			level, err := apmlog.ParseLogLevel(v)
			if err != nil {
				errorf("central config failure: %s", err)
				delete(attrs, k)
				continue
			}
			if dl := apmlog.DefaultLogger(); dl != nil && dl == logger {
				updates = append(updates, func(*instrumentationConfig) {
					dl.SetLevel(level)
				})
			} else {
				warningf("central config ignored: %s set to %s, but custom logger in use", k, v)
				delete(attrs, k)
				continue
			}
		case envSpanCompressionEnabled:
			val, err := strconv.ParseBool(v)
			if err != nil {
				errorf("central config failure: failed to parse %s: %s", k, err)
				delete(attrs, k)
				continue
			}
			updates = append(updates, func(cfg *instrumentationConfig) {
				cfg.compressionOptions.enabled = val
			})
		case envSpanCompressionExactMatchMaxDuration:
			duration, err := configutil.ParseDuration(v)
			if err != nil {
				errorf("central config failure: failed to parse %s: %s", k, err)
				delete(attrs, k)
				continue
			}
			updates = append(updates, func(cfg *instrumentationConfig) {
				cfg.compressionOptions.exactMatchMaxDuration = duration
			})
		case envSpanCompressionSameKindMaxDuration:
			duration, err := configutil.ParseDuration(v)
			if err != nil {
				errorf("central config failure: failed to parse %s: %s", k, err)
				delete(attrs, k)
				continue
			}
			updates = append(updates, func(cfg *instrumentationConfig) {
				cfg.compressionOptions.sameKindMaxDuration = duration
			})
		default:
			warningf("central config failure: unsupported config: %s", k)
			delete(attrs, k)
			continue
		}
		debugf("central config update: updated %s to %s", k, v)
	}
	for k := range old {
		if _, ok := attrs[k]; ok {
			continue
		}
		updates = append(updates, func(cfg *instrumentationConfig) {
			if f, ok := cfg.local[envName(k)]; ok {
				f(&cfg.instrumentationConfigValues)
			}
		})
		debugf("central config update: reverted %s to local config", k)
	}
	if updates != nil {
		remote := make(map[string]struct{})
		for k := range attrs {
			remote[envName(k)] = struct{}{}
		}
		t.updateInstrumentationConfig(func(cfg *instrumentationConfig) {
			cfg.remote = remote
			for _, update := range updates {
				update(cfg)
			}
		})
	}
}

// instrumentationConfig returns the current instrumentationConfig.
//
// The returned value is immutable.
func (t *Tracer) instrumentationConfig() *instrumentationConfig {
	config := atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&t.instrumentationConfigInternal)))
	return (*instrumentationConfig)(config)
}

// setLocalInstrumentationConfig sets local transaction configuration with
// the specified environment variable key.
func (t *Tracer) setLocalInstrumentationConfig(envKey string, f func(cfg *instrumentationConfigValues)) {
	t.updateInstrumentationConfig(func(cfg *instrumentationConfig) {
		cfg.local[envKey] = f
		if _, ok := cfg.remote[envKey]; !ok {
			f(&cfg.instrumentationConfigValues)
		}
	})
}

func (t *Tracer) updateInstrumentationConfig(f func(cfg *instrumentationConfig)) {
	for {
		oldConfig := t.instrumentationConfig()
		newConfig := *oldConfig
		f(&newConfig)
		if atomic.CompareAndSwapPointer(
			(*unsafe.Pointer)(unsafe.Pointer(&t.instrumentationConfigInternal)),
			unsafe.Pointer(oldConfig),
			unsafe.Pointer(&newConfig),
		) {
			return
		}
	}
}

// IgnoredTransactionURL returns whether the given transaction URL should be ignored
func (t *Tracer) IgnoredTransactionURL(url *url.URL) bool {
	return t.instrumentationConfig().ignoreTransactionURLs.MatchAny(url.String())
}

// instrumentationConfig holds current configuration values, as well as information
// required to revert from remote to local configuration.
type instrumentationConfig struct {
	instrumentationConfigValues

	// local holds functions for setting instrumentationConfigValues to the most
	// recently, locally specified configuration.
	local map[string]func(*instrumentationConfigValues)

	// remote holds the environment variable keys for applied remote config.
	remote map[string]struct{}
}

// instrumentationConfigValues holds configuration that is accessible outside of the
// tracer loop, for instrumentation: StartTransaction, StartSpan, CaptureError, etc.
//
// NOTE(axw) when adding configuration here, you must also update `newTracer` to
// set the initial entry in instrumentationConfig.local, in order to properly reset
// to the local value, even if the default is the zero value.
type instrumentationConfigValues struct {
	recording                 bool
	captureBody               CaptureBodyMode
	captureHeaders            bool
	maxSpans                  int
	sampler                   Sampler
	spanStackTraceMinDuration time.Duration
	exitSpanMinDuration       time.Duration
	continuationStrategy      string
	stackTraceLimit           int
	propagateLegacyHeader     bool
	sanitizedFieldNames       wildcard.Matchers
	ignoreTransactionURLs     wildcard.Matchers
	compressionOptions        compressionOptions
}
