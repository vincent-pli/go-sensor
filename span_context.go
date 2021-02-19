// (c) Copyright IBM Corp. 2021
// (c) Copyright Instana Inc. 2020

package instana

import (
	"strings"

	"github.com/instana/go-sensor/w3ctrace"
)

// EUMCorrelationData represents the data sent by the Instana End-User Monitoring script
// integrated into frontend
type EUMCorrelationData struct {
	Type string
	ID   string
}

// SpanReference is a reference to a span, possibly belonging to another trace, that is relevant
// to the span context
type SpanReference struct {
	TraceID string
	SpanID  string
}

// SpanContext holds the basic Span metadata.
type SpanContext struct {
	// The higher 4 bytes of a 128-bit trace ID
	TraceIDHi int64
	// A probabilistically unique identifier for a [multi-span] trace.
	TraceID int64
	// A probabilistically unique identifier for a span.
	SpanID int64
	// An optional parent span ID, 0 if this is the root span context.
	ParentID int64
	// Optional links to traces relevant to this context, i.e. an indirect parent
	Links []SpanReference
	// Whether the trace is sampled.
	Sampled bool
	// Whether the trace is suppressed and should not be sent to the agent.
	Suppressed bool
	// The span's associated baggage.
	Baggage map[string]string // initialized on first use
	// The W3C trace context
	W3CContext w3ctrace.Context
	// Whether the used trace ID came from 3rd party, e.g. W3C Trace Context
	ForeignTrace bool
	// Correlation is the correlation data sent by the frontend EUM script
	Correlation EUMCorrelationData
}

// NewRootSpanContext initializes a new root span context issuing a new trace ID
func NewRootSpanContext() SpanContext {
	spanID := randomID()

	c := SpanContext{
		TraceID: spanID,
		SpanID:  spanID,
	}

	c.W3CContext = newW3CTraceContext(c)

	return c
}

// NewSpanContext initializes a new child span context from its parent. It will
// ignore the parent context if it contains neither Instana trace and span IDs
// nor a W3C trace context
func NewSpanContext(parent SpanContext) SpanContext {
	var foreignTrace bool
	if parent.TraceIDHi == 0 && parent.TraceID == 0 && parent.SpanID == 0 {
		parent = restoreFromW3CTraceContext(parent.W3CContext)
		foreignTrace = true && !sensor.options.disableW3CTraceCorrelation
	}

	if parent.TraceIDHi == 0 && parent.TraceID == 0 && parent.SpanID == 0 {
		c := NewRootSpanContext()

		// preserve the W3C trace context even if it was not used
		if !parent.W3CContext.IsZero() {
			c.W3CContext = parent.W3CContext
		}

		return c
	}

	c := parent.Clone()
	c.SpanID, c.ParentID = randomID(), parent.SpanID
	c.ForeignTrace = foreignTrace

	// initialize W3C trace context if it's not set already
	if c.W3CContext.IsZero() {
		c.W3CContext = newW3CTraceContext(c)
		return c
	}

	// update W3C trace context parent
	w3cParent := c.W3CContext.Parent()
	w3cParent.ParentID = FormatID(c.SpanID)
	c.W3CContext.RawParent = w3cParent.String()

	// check if there is Instana state stored in the W3C tracestate header
	if foreignTrace {
		w3cState := c.W3CContext.State()
		if ancestor, ok := w3cState.Fetch(w3ctrace.VendorInstana); ok {
			if ref, ok := parseW3CInstanaState(ancestor); ok {
				c.Links = append(c.Links, ref)
			}
		}
	}

	return c
}

func restoreFromW3CTraceContext(trCtx w3ctrace.Context) SpanContext {
	if trCtx.IsZero() {
		return SpanContext{}
	}

	if sensor.options.disableW3CTraceCorrelation {
		return restoreFromW3CTraceState(trCtx)
	}

	parent := trCtx.Parent()

	traceIDHi, traceIDLo, err := ParseLongID(parent.TraceID)
	if err != nil {
		return SpanContext{}
	}

	parentID, err := ParseID(parent.ParentID)
	if err != nil {
		return SpanContext{}
	}

	return SpanContext{
		TraceIDHi:  traceIDHi,
		TraceID:    traceIDLo,
		SpanID:     parentID,
		Suppressed: !parent.Flags.Sampled,
		W3CContext: trCtx,
	}
}

func restoreFromW3CTraceState(trCtx w3ctrace.Context) SpanContext {
	if trCtx.IsZero() {
		return SpanContext{}
	}

	c := SpanContext{
		W3CContext: trCtx,
	}

	state, ok := trCtx.State().Fetch(w3ctrace.VendorInstana)
	if !ok {
		return c
	}

	ref, ok := parseW3CInstanaState(state)
	if !ok {
		return c
	}

	traceIDHi, traceIDLo, err := ParseLongID(ref.TraceID)
	if err != nil {
		return c
	}

	parentID, err := ParseID(ref.SpanID)
	if err != nil {
		return c
	}

	c.TraceIDHi, c.TraceID, c.SpanID = traceIDHi, traceIDLo, parentID

	return c
}

// ForeachBaggageItem belongs to the opentracing.SpanContext interface
func (c SpanContext) ForeachBaggageItem(handler func(k, v string) bool) {
	for k, v := range c.Baggage {
		if !handler(k, v) {
			break
		}
	}
}

// WithBaggageItem returns an entirely new SpanContext with the
// given key:value baggage pair set.
func (c SpanContext) WithBaggageItem(key, val string) SpanContext {
	res := c.Clone()

	if res.Baggage == nil {
		res.Baggage = make(map[string]string, 1)
	}
	res.Baggage[key] = val

	return res
}

// Clone returns a deep copy of a SpanContext
func (c SpanContext) Clone() SpanContext {
	res := SpanContext{
		TraceIDHi:  c.TraceIDHi,
		TraceID:    c.TraceID,
		SpanID:     c.SpanID,
		ParentID:   c.ParentID,
		Sampled:    c.Sampled,
		Suppressed: c.Suppressed,
		W3CContext: c.W3CContext,
	}

	if c.Baggage != nil {
		res.Baggage = make(map[string]string, len(c.Baggage))
		for k, v := range c.Baggage {
			res.Baggage[k] = v
		}
	}

	return res
}

func newW3CTraceContext(c SpanContext) w3ctrace.Context {
	return w3ctrace.New(w3ctrace.Parent{
		Version:  w3ctrace.Version_Max,
		TraceID:  FormatLongID(c.TraceIDHi, c.TraceID),
		ParentID: FormatID(c.SpanID),
		Flags: w3ctrace.Flags{
			Sampled: !c.Suppressed,
		},
	})
}

func parseW3CInstanaState(vendorData string) (ancestor SpanReference, ok bool) {
	ind := strings.IndexByte(vendorData, ';')
	if ind < 0 {
		return SpanReference{}, false
	}

	return SpanReference{
		TraceID: vendorData[:ind],
		SpanID:  vendorData[ind+1:],
	}, true
}
