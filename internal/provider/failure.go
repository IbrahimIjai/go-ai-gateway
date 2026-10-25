package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// FailureClass says why an upstream call failed, which decides whether
// another attempt can help. The taxonomy follows Bifrost's failure classes
// (core/schemas/failureclass.go).
type FailureClass string

const (
	ClassTransient   FailureClass = "transient"    // 5xx, network errors, cut streams
	ClassRateLimit   FailureClass = "rate_limit"   // 429 without a quota signal
	ClassQuota       FailureClass = "quota"        // provider account out of credit or over a spend cap
	ClassCredential  FailureClass = "credential"   // 401/403: the gateway's provider key was refused
	ClassModelGone   FailureClass = "model_gone"   // 404: model or endpoint unknown upstream
	ClassCallerFault FailureClass = "caller_fault" // other 4xx: the request itself is invalid
	ClassTimeout     FailureClass = "timeout"      // 408 and deadlines: network, idle or first event
	ClassCancelled   FailureClass = "cancelled"    // the client went away
	ClassUnsupported FailureClass = "unsupported"  // this provider cannot honor a request feature; another may
	ClassAdmission   FailureClass = "admission"    // refused before any upstream call: open breaker, full bulkhead, disabled
	ClassUnknown     FailureClass = "unknown"      // malformed responses and gateway bugs
)

// UnsupportedError reports a request feature a provider cannot honor. The
// request is refused rather than sent without the feature.
type UnsupportedError struct {
	Provider string
	Feature  string
}

func (e *UnsupportedError) Error() string { return e.Provider + " does not support " + e.Feature }

// AdmissionError means the gateway refused to call a deployment at all, so
// nothing was sent upstream and nothing can have been billed.
type AdmissionError struct {
	Deployment string
	Reason     string // disabled | breaker_open | concurrency_full
}

func (e *AdmissionError) Error() string {
	return "deployment " + e.Deployment + " not admitted: " + e.Reason
}

// streamError is a failure a provider reported inside an HTTP 200 stream.
type streamError struct {
	class FailureClass
	code  string
}

func (e *streamError) Error() string {
	return fmt.Sprintf("upstream stream error (%s): %s", e.class, e.code)
}

// newStreamError classifies an error event from a stream. Providers name
// these with the codes they use for HTTP errors; Gemini adds the HTTP status.
func newStreamError(body []byte) error {
	code, message := providerError(body)
	class := classifyCode(code, message)
	var v struct {
		Error struct {
			Code any `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &v) == nil {
		if status, ok := v.Error.Code.(float64); ok && status >= 400 {
			class = classify(int(status), code, message)
		}
	}
	return &streamError{class: class, code: code}
}

// APIError is an upstream HTTP error. Message is the status text: upstream
// bodies are read only to classify the failure, never echoed or logged.
type APIError struct {
	Status     int
	Message    string
	Class      FailureClass  // zero: derived from Status and Code
	Code       string        // provider error code or type, e.g. insufficient_quota
	RetryAfter time.Duration // from retry-after-ms or Retry-After; zero if absent
}

func (e *APIError) Error() string {
	return fmt.Sprintf("upstream HTTP %d (%s): %s", e.Status, e.class(), e.Message)
}

func (e *APIError) class() FailureClass {
	if e.Class != "" {
		return e.Class
	}
	return classify(e.Status, e.Code, "")
}

// Class reports the failure class of an upstream call's error.
func Class(err error) FailureClass {
	if err == nil {
		return ""
	}
	var api *APIError
	var inStream *streamError
	var unsupported *UnsupportedError
	var admission *AdmissionError
	var netErr net.Error
	switch {
	case errors.As(err, &api):
		return api.class()
	case errors.As(err, &inStream):
		return inStream.class
	case errors.As(err, &unsupported):
		return ClassUnsupported
	case errors.As(err, &admission):
		return ClassAdmission
	case errors.Is(err, context.Canceled):
		return ClassCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout
	case errors.Is(err, io.ErrUnexpectedEOF):
		return ClassTransient
	case errors.As(err, &netErr):
		if netErr.Timeout() {
			return ClassTimeout
		}
		return ClassTransient
	}
	return ClassUnknown
}

// Retryable reports whether another deployment may succeed where this attempt
// failed: the fault lies with the provider, not the request or the client.
func Retryable(err error) bool {
	switch Class(err) {
	case ClassTransient, ClassRateLimit, ClassTimeout, ClassQuota, ClassCredential, ClassModelGone, ClassUnsupported, ClassAdmission:
		return true
	}
	return false
}

// quotaMarkers say the provider account is out of credit or over a spend cap,
// which providers report under 400, 403 or 429. From Bifrost's quotaMarkers
// (core/failureclass.go).
var quotaMarkers = []string{
	"credit balance", "spend limit", "api usage limits", "prepayment credits", "credits are depleted",
	"spending cap", "requires billing to be enabled", "key limit exceeded", "account is not active",
}

// classify maps a status, the provider's error code and its lowercased message
// to a class. Quota is checked before rate limit because OpenAI reports an
// exhausted balance as a 429 too; a 5xx stays transient whatever its body says.
func classify(status int, code, message string) FailureClass {
	switch {
	case status >= 500:
		return ClassTransient
	case status == http.StatusPaymentRequired, code == "insufficient_quota", code == "blocked_api_access", containsAny(message, quotaMarkers):
		return ClassQuota
	case status == http.StatusTooManyRequests:
		return ClassRateLimit
	case status == http.StatusRequestTimeout:
		return ClassTimeout
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return ClassCredential
	case status == http.StatusNotFound:
		return ClassModelGone
	case status >= 400:
		return ClassCallerFault
	}
	return ClassUnknown
}

// classifyCode maps a provider error code that arrived without an HTTP status,
// as in a stream error event.
func classifyCode(code, message string) FailureClass {
	switch {
	case code == "insufficient_quota", code == "blocked_api_access", containsAny(message, quotaMarkers):
		return ClassQuota
	case strings.Contains(code, "rate_limit"), code == "RESOURCE_EXHAUSTED":
		return ClassRateLimit
	case code == "DEADLINE_EXCEEDED":
		return ClassTimeout
	case code == "overloaded_error", code == "api_error", code == "server_error", code == "UNAVAILABLE", code == "INTERNAL":
		return ClassTransient
	}
	return ClassUnknown
}

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func newAPIError(resp *http.Response, body []byte, now time.Time) *APIError {
	code, message := providerError(body)
	return &APIError{
		Status:     resp.StatusCode,
		Message:    http.StatusText(resp.StatusCode),
		Class:      classify(resp.StatusCode, code, message),
		Code:       code,
		RetryAfter: retryAfter(resp.Header, now),
	}
}

// providerError reads the error code and message from OpenAI, Anthropic and
// Gemini error bodies. The message is only matched against quota markers.
func providerError(body []byte) (code, message string) {
	var v struct {
		Error struct {
			Type    string `json:"type"`   // OpenAI and Anthropic
			Code    any    `json:"code"`   // OpenAI: a string; Gemini: the HTTP status
			Status  string `json:"status"` // Gemini, e.g. RESOURCE_EXHAUSTED
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &v) != nil {
		return "", ""
	}
	message = strings.ToLower(v.Error.Message)
	openAICode, _ := v.Error.Code.(string)
	for _, c := range []string{openAICode, v.Error.Type, v.Error.Status} {
		if c = errorCode(c); c != "" {
			return c, message
		}
	}
	return "", message
}

// errorCode keeps s only if it looks like a provider error code: those are
// safe to log and store, while free text from upstream is not.
func errorCode(s string) string {
	if len(s) > 64 {
		return ""
	}
	for _, r := range s {
		if !(r == '_' || r == '-' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return ""
		}
	}
	return s
}

// maxRetryAfter caps a provider's requested wait. Whether to wait at all is
// the retry loop's decision.
const maxRetryAfter = time.Hour

// retryAfter reads retry-after-ms (OpenAI) or Retry-After as seconds or an
// HTTP date. Zero means the provider gave no hint.
func retryAfter(h http.Header, now time.Time) time.Duration {
	if ms, err := strconv.ParseInt(strings.TrimSpace(h.Get("Retry-After-Ms")), 10, 64); err == nil && ms > 0 {
		return capWait(ms, time.Millisecond)
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if s, err := strconv.ParseInt(v, 10, 64); err == nil && s > 0 {
		return capWait(s, time.Second)
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return min(t.Sub(now), maxRetryAfter)
	}
	return 0
}

// capWait converts n units to a duration of at most maxRetryAfter, without
// overflowing on absurd values.
func capWait(n int64, unit time.Duration) time.Duration {
	if n > int64(maxRetryAfter/unit) {
		return maxRetryAfter
	}
	return time.Duration(n) * unit
}
