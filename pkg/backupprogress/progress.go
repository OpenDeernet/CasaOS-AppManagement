// Package backupprogress carries where a running copy is, from the engine that copies to
// whoever follows it.
//
// The callback rides on the context so the copy interfaces stay as they are: an engine that
// finds one reports to it, and one that does not costs nothing.
package backupprogress

import "context"

// Stats is how far one copy has got, as its engine counts.
type Stats struct {
	Bytes int64 `json:"bytes"`
	// TotalBytes zero means the engine is still taking stock.
	TotalBytes int64 `json:"total_bytes,omitempty"`
	Files      int64 `json:"files"`
	TotalFiles int64 `json:"total_files,omitempty"`
	// Speed is in bytes a second.
	Speed int64 `json:"speed,omitempty"`
	// ETA is in seconds; nil when the engine cannot tell.
	ETA     *int64 `json:"eta,omitempty"`
	Current string `json:"current,omitempty"`
}

type key struct{}

// With returns a context whose copies report to report.
func With(ctx context.Context, report func(Stats)) context.Context {
	return context.WithValue(ctx, key{}, report)
}

// Report hands stats to the context's callback, if it has one.
func Report(ctx context.Context, stats Stats) {
	if report, ok := ctx.Value(key{}).(func(Stats)); ok {
		report(stats)
	}
}

// Wanted says whether anybody follows this copy, so an engine asks for figures only then.
func Wanted(ctx context.Context) bool {
	_, ok := ctx.Value(key{}).(func(Stats))

	return ok
}
