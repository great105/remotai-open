// Package observability wires error reporting (Sentry) and runtime visibility.
//
// Activation is opt-in via env var SENTRY_DSN_GO. If unset, all functions are
// no-ops and zero overhead is paid.
package observability

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/getsentry/sentry-go"

	"tgcontrol/internal/version"
)

// initialized tracks whether Sentry was successfully initialized.
var initialized bool

// Init initializes the Sentry SDK if SENTRY_DSN_GO is set in the environment.
// Returns true if Sentry is active, false otherwise (e.g. no DSN).
//
// Caller should defer Close() to flush buffered events on shutdown.
func Init() bool {
	dsn := os.Getenv("SENTRY_DSN_GO")
	if dsn == "" {
		return false
	}

	env := os.Getenv("SENTRY_ENV")
	if env == "" {
		env = "production"
	}

	err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Release:          "tgcontrol@" + version.Version,
		Environment:      env,
		AttachStacktrace: true,
		// Performance: disabled by default — set SENTRY_TRACES_RATE=0.1 to enable.
		TracesSampleRate: tracesRate(),
		// Drop noisy errors at the source.
		BeforeSend: func(event *sentry.Event, hint *sentry.EventHint) *sentry.Event {
			if hint != nil && hint.OriginalException != nil {
				if isIgnorable(hint.OriginalException) {
					return nil
				}
			}
			return event
		},
	})
	if err != nil {
		log.Printf("Sentry init failed: %v", err)
		return false
	}

	initialized = true
	log.Printf("Sentry enabled (env=%s, release=tgcontrol@%s)", env, version.Version)
	return true
}

// Close flushes pending events. Safe to call even if Init was not called.
func Close() {
	if !initialized {
		return
	}
	sentry.Flush(2 * time.Second)
}

// RecoverPanic is a deferred panic guard for background goroutines and handlers
// that must NOT take the whole process down. It logs the stack, saves a crash
// report to disk, reports to Sentry (if enabled), and SWALLOWS the panic so the
// surrounding goroutine simply exits while the rest of the app keeps running.
//
// Use as: defer observability.RecoverPanic("goroutine name")
//
// (For the main goroutine, where a crash should be fatal-with-report, use
// InstallGlobal instead — it re-panics on purpose.)
func RecoverPanic(label string) {
	if r := recover(); r != nil {
		stack := debug.Stack()
		log.Printf("[PANIC] recovered in %s: %v\n%s", label, r, stack)
		SaveCrashReport(r, map[string]string{"site": label})
		if initialized {
			hub := sentry.CurrentHub().Clone()
			hub.WithScope(func(scope *sentry.Scope) {
				scope.SetTag("goroutine", label)
				scope.SetContext("panic", sentry.Context{"stack": string(stack)})
				switch v := r.(type) {
				case error:
					hub.CaptureException(v)
				default:
					hub.CaptureMessage("panic: " + label)
				}
			})
			sentry.Flush(2 * time.Second)
		}
		// Intentionally no re-panic: the goroutine exits, the process survives.
	}
}

// HTTPMiddleware wraps an http.Handler to recover panics and report them.
// The recover runs UNCONDITIONALLY (even when Sentry is disabled) — a handler
// panic must never crash the whole process; only the optional Sentry reporting
// is gated on initialization.
func HTTPMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		var hub *sentry.Hub
		if initialized {
			hub = sentry.CurrentHub().Clone()
			hub.Scope().SetRequest(r)
			ctx = sentry.SetHubOnContext(ctx, hub)
		}

		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			stack := debug.Stack()
			log.Printf("[PANIC] recovered in HTTP %s %s: %v\n%s", r.Method, r.URL.Path, rec, stack)
			SaveCrashReport(rec, map[string]string{"site": "http", "method": r.Method, "path": r.URL.Path})
			if hub != nil {
				hub.WithScope(func(scope *sentry.Scope) {
					scope.SetContext("panic", sentry.Context{"stack": string(stack)})
					switch v := rec.(type) {
					case error:
						hub.CaptureException(v)
					default:
						hub.CaptureMessage("http panic")
					}
				})
				sentry.Flush(2 * time.Second)
			}
			// Don't leak the panic to the client.
			if w != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()

		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isIgnorable filters out errors that are too noisy to be useful.
func isIgnorable(err error) bool {
	if err == nil {
		return true
	}
	// context.Canceled is a normal shutdown signal.
	if errors.Is(err, context.Canceled) {
		return true
	}
	return false
}

func tracesRate() float64 {
	raw := os.Getenv("SENTRY_TRACES_RATE")
	if raw == "" {
		return 0.0
	}
	// Best-effort parse; invalid → disabled.
	switch raw {
	case "1", "1.0":
		return 1.0
	case "0.5":
		return 0.5
	case "0.1":
		return 0.1
	case "0.01":
		return 0.01
	default:
		return 0.0
	}
}
