package service

import (
	"context"
	"time"
)

// HTTPUpstreamProfile marks HTTP upstream requests that need provider-specific
// transport policy.
type HTTPUpstreamProfile string

const (
	HTTPUpstreamProfileDefault HTTPUpstreamProfile = ""
	HTTPUpstreamProfileOpenAI  HTTPUpstreamProfile = "openai"
	HTTPUpstreamProfileNVIDIA  HTTPUpstreamProfile = "nvidia"
)

type httpUpstreamProfileContextKey struct{}
type httpUpstreamDisableRedirectsContextKey struct{}
type httpUpstreamNvidiaAccountSwitchStartedAtContextKey struct{}

// WithHTTPUpstreamProfile injects an upstream transport profile into ctx.
func WithHTTPUpstreamProfile(ctx context.Context, profile HTTPUpstreamProfile) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if profile == HTTPUpstreamProfileDefault {
		return ctx
	}
	return context.WithValue(ctx, httpUpstreamProfileContextKey{}, profile)
}

// HTTPUpstreamProfileFromContext resolves the upstream transport profile from ctx.
func HTTPUpstreamProfileFromContext(ctx context.Context) HTTPUpstreamProfile {
	if ctx == nil {
		return HTTPUpstreamProfileDefault
	}
	profile, ok := ctx.Value(httpUpstreamProfileContextKey{}).(HTTPUpstreamProfile)
	if !ok {
		return HTTPUpstreamProfileDefault
	}
	switch profile {
	case HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileNVIDIA:
		return profile
	default:
		return HTTPUpstreamProfileDefault
	}
}

// WithHTTPUpstreamRedirectsDisabled prevents credential-bearing probes from
// following redirects through the shared upstream client.
func WithHTTPUpstreamRedirectsDisabled(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, httpUpstreamDisableRedirectsContextKey{}, true)
}

func HTTPUpstreamRedirectsDisabled(ctx context.Context) bool {
	return ctx != nil && ctx.Value(httpUpstreamDisableRedirectsContextKey{}) == true
}

// WithNVIDIAAccountSwitchStartedAt records the timestamp captured immediately
// after a replacement NVIDIA account is successfully selected. The caller
// stores startedAt at that selection point; downstream code uses it to measure
// the span from replacement-selected to the first successful http.Client
// WroteRequest. It is not stamped at failure detection or switch decision.
// Zero values are not retained.
func WithNVIDIAAccountSwitchStartedAt(ctx context.Context, startedAt time.Time) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if startedAt.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, httpUpstreamNvidiaAccountSwitchStartedAtContextKey{}, startedAt)
}

// NVIDIAAccountSwitchStartedAt returns the timestamp captured at
// replacement-selected time and whether it is present. The span from this
// timestamp to the first successful WroteRequest measures replacement-account
// reuse latency; absence is a signal that no replacement occurred for this
// request and the value must not be used as a baseline.
func NVIDIAAccountSwitchStartedAt(ctx context.Context) (time.Time, bool) {
	if ctx == nil {
		return time.Time{}, false
	}
	startedAt, ok := ctx.Value(httpUpstreamNvidiaAccountSwitchStartedAtContextKey{}).(time.Time)
	if !ok || startedAt.IsZero() {
		return time.Time{}, false
	}
	return startedAt, true
}
