// Package transport sends collected samples to the backend and parses the response.
package transport

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"metrics-agent/internal/metrics"
)

// Limits the agent must enforce itself, without relying on a 4xx from the server (plan item 4.4).
const (
	// MaxSamples is how many samples fit into a single batch.
	MaxSamples = 60
	// MaxFilesystems is how many mount points fit into a single sample.
	// The same limit the collector trims by: two independent constants would
	// drift apart silently, and trimFilesystems would quietly cut a valid sample.
	MaxFilesystems = metrics.DefaultMaxFilesystems
	// MaxBodyBytes is the cap on the request body in the form it goes over the network.
	MaxBodyBytes = 128 << 10
	// GzipThreshold is the size from which the body is compressed.
	GzipThreshold = 2048
)

// DefaultTimeout is the timeout for the whole request. Noticeably smaller than report_interval,
// so that a hung request does not eat the next tick.
const DefaultTimeout = 10 * time.Second

// idleConnTimeout is deliberately modest: keepalive_timeout on nginx is short (plan item 4.2).
const idleConnTimeout = 5 * time.Second

// maxResponseBytes is how many response bytes we read: by contract the response is tiny.
const maxResponseBytes = 64 << 10

// Request is the request body per the contract in plan item 4.1.
type Request struct {
	AgentVersion string            `json:"agent_version"`
	Hostname     string            `json:"hostname"`
	OS           string            `json:"os"`
	BootID       string            `json:"boot_id"`
	Samples      []*metrics.Sample `json:"samples"`
}

// Response is the backend response. The pointer on ClockSkew distinguishes "no divergence"
// from "the field was absent from the response".
type Response struct {
	OK           bool     `json:"ok"`
	NextReportIn int      `json:"next_report_in"`
	ServerTime   int64    `json:"server_time"`
	Degraded     bool     `json:"degraded"`
	Suspended    bool     `json:"suspended"`
	ClockSkew    *float64 `json:"clock_skew"`
}

// Kind tells the caller how to treat the sample after a failed send.
type Kind int

const (
	// KindRetry: 5xx, timeout, network error: keep the data, retry the attempt.
	KindRetry Kind = iota
	// KindDrop: 4xx other than 401 and 429: the server will never accept this batch, drop it.
	KindDrop
	// KindRateLimit: 429: keep the data, wait for Retry-After.
	KindRateLimit
	// KindUnauthorized: 401: the token was rejected, switch to sparse retries.
	KindUnauthorized
	// KindTooLarge: the body did not fit MaxBodyBytes: the batch must be split, not dropped.
	// The server never saw this data, it must not be lost.
	KindTooLarge
)

// errBodyTooLarge distinguishes exceeding the body limit from other encoding errors:
// the former is cured by splitting the batch, the latter is not.
var errBodyTooLarge = errors.New("request body is larger than the limit")

// Error is a failed send together with the decision about the fate of the data.
type Error struct {
	Kind       Kind
	Status     int
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	switch {
	case e.Err != nil && e.Status != 0:
		return fmt.Sprintf("send failed: HTTP %d: %v", e.Status, e.Err)
	case e.Err != nil:
		return fmt.Sprintf("send failed: %v", e.Err)
	default:
		return fmt.Sprintf("send failed: HTTP %d", e.Status)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// Client sends batches to the endpoint. One send is one attempt:
// the retry policy lives above, in the agent.
type Client struct {
	log       *slog.Logger
	http      *http.Client
	endpoint  string
	token     string
	userAgent string
}

// NewClient assembles a client with reusable connections and a hard timeout.
// Certificate verification is always on and cannot be disabled by a flag or the config.
func NewClient(log *slog.Logger, endpoint, token, userAgent string) *Client {
	return &Client{
		log:       log,
		endpoint:  endpoint,
		token:     token,
		userAgent: userAgent,
		http: &http.Client{
			Timeout: DefaultTimeout,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout: 5 * time.Second,
				TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     idleConnTimeout,
			},
		},
	}
}

// Send performs a single send attempt. The error is always of type *Error.
func (c *Client) Send(ctx context.Context, req *Request) (*Response, error) {
	body, gzipped, err := encode(req)
	if err != nil {
		kind := KindDrop
		if errors.Is(err, errBodyTooLarge) {
			kind = KindTooLarge
		}
		return nil, &Error{Kind: kind, Err: err}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Kind: KindDrop, Err: err}
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", c.userAgent)
	if gzipped {
		httpReq.Header.Set("Content-Encoding", "gzip")
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// A network error and a timeout are indistinguishable in consequence: keep the data, try again.
		return nil, &Error{Kind: KindRetry, Err: err}
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	// Drain the tail, otherwise the connection will not return to the pool.
	_, _ = io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode/100 == 2:
		return decodeResponse(c.log, raw), nil
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, &Error{Kind: KindUnauthorized, Status: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &Error{
			Kind:       KindRateLimit,
			Status:     resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	case resp.StatusCode/100 == 4:
		return nil, &Error{Kind: KindDrop, Status: resp.StatusCode}
	default:
		return nil, &Error{Kind: KindRetry, Status: resp.StatusCode}
	}
}

// decodeResponse parses the body of a successful response. An unreadable body is no reason
// to consider the sample rejected: the server already said 2xx.
func decodeResponse(log *slog.Logger, raw []byte) *Response {
	out := &Response{OK: true}
	if len(bytes.TrimSpace(raw)) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, out); err != nil {
		log.Warn("failed to parse the server response", "err", err)
		return &Response{OK: true}
	}
	return out
}

// encode serializes the request, compresses bodies larger than GzipThreshold and checks the limits.
func encode(req *Request) (body []byte, gzipped bool, err error) {
	if len(req.Samples) > MaxSamples {
		return nil, false, fmt.Errorf("the batch has %d samples, maximum is %d", len(req.Samples), MaxSamples)
	}

	raw, err := json.Marshal(trimFilesystems(req))
	if err != nil {
		return nil, false, fmt.Errorf("failed to serialize the request: %w", err)
	}

	body = raw
	if len(raw) > GzipThreshold {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(raw); err != nil {
			return nil, false, fmt.Errorf("failed to compress the body: %w", err)
		}
		if err := zw.Close(); err != nil {
			return nil, false, fmt.Errorf("failed to compress the body: %w", err)
		}
		body, gzipped = buf.Bytes(), true
	}

	if len(body) > MaxBodyBytes {
		return nil, false, fmt.Errorf("%w: %d bytes, maximum is %d", errBodyTooLarge, len(body), MaxBodyBytes)
	}
	return body, gzipped, nil
}

// trimFilesystems trims the filesystem list to the contract limit without touching the caller's sample.
func trimFilesystems(req *Request) *Request {
	var samples []*metrics.Sample
	for i, s := range req.Samples {
		if s == nil || len(s.FS) <= MaxFilesystems {
			continue
		}
		if samples == nil {
			samples = append(samples, req.Samples...)
		}
		trimmed := *s
		trimmed.FS = s.FS[:MaxFilesystems]
		samples[i] = &trimmed
	}
	if samples == nil {
		return req
	}
	out := *req
	out.Samples = samples
	return &out
}

// parseRetryAfter understands both header forms: seconds and an HTTP date.
func parseRetryAfter(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	if sec, err := strconv.Atoi(value); err == nil {
		if sec <= 0 {
			return 0
		}
		return time.Duration(sec) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
