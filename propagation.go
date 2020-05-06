package instana

import (
	"net/http"
	"strings"

	"github.com/instana/go-sensor/w3ctrace"
	ot "github.com/opentracing/opentracing-go"
)

// Instana header constants
const (
	// FieldT Trace ID header
	FieldT = "x-instana-t"
	// FieldS Span ID header
	FieldS = "x-instana-s"
	// FieldL Level header
	FieldL = "x-instana-l"
	// FieldB OT Baggage header
	FieldB = "x-instana-b-"
)

func injectTraceContext(sc SpanContext, opaqueCarrier interface{}) error {
	roCarrier, ok := opaqueCarrier.(ot.TextMapReader)
	if !ok {
		return ot.ErrInvalidCarrier
	}

	// Handle pre-existing case-sensitive keys
	exstfieldT := FieldT
	exstfieldS := FieldS
	exstfieldL := FieldL
	exstfieldB := FieldB

	roCarrier.ForeachKey(func(k, v string) error {
		switch strings.ToLower(k) {
		case FieldT:
			exstfieldT = k
		case FieldS:
			exstfieldS = k
		case FieldL:
			exstfieldL = k
		default:
			if strings.HasPrefix(strings.ToLower(k), FieldB) {
				exstfieldB = string([]rune(k)[:len(FieldB)])
			}
		}
		return nil
	})

	carrier, ok := opaqueCarrier.(ot.TextMapWriter)
	if !ok {
		return ot.ErrInvalidCarrier
	}

	if c, ok := opaqueCarrier.(ot.HTTPHeadersCarrier); ok {
		// Even though the godoc claims that the key passed to (*http.Header).Set()
		// is case-insensitive, it actually normalizes it using textproto.CanonicalMIMEHeaderKey()
		// before populating the value. As a result headers with non-canonical will not be
		// overwritted with a new value. This is only the case if header names were set while
		// initializing the http.Header instance, i.e.
		//     h := http.Headers{"X-InStAnA-T": {"abc123"}}
		// and does not apply to a common case when requests are being created using http.NewRequest()
		// or http.ReadRequest() that call (*http.Header).Set() to set header values.
		h := http.Header(c)
		delete(h, exstfieldT)
		delete(h, exstfieldS)
		delete(h, exstfieldL)

		for key := range h {
			if strings.HasPrefix(strings.ToLower(key), FieldB) {
				delete(h, key)
			}
		}

		addW3CTraceContext(h, sc)
	}

	carrier.Set(exstfieldT, FormatID(sc.TraceID))
	carrier.Set(exstfieldS, FormatID(sc.SpanID))
	carrier.Set(exstfieldL, formatLevel(sc))

	for k, v := range sc.Baggage {
		carrier.Set(exstfieldB+k, v)
	}

	return nil
}

func addW3CTraceContext(h http.Header, sc SpanContext) {
	traceID, spanID := FormatID(sc.TraceID), FormatID(sc.SpanID)

	trCtx, ok := sc.ForeignParent.(w3ctrace.Context)
	if !ok {
		trCtx = w3ctrace.New(w3ctrace.Parent{
			Version:  w3ctrace.Version_Max,
			TraceID:  traceID,
			ParentID: spanID,
			Flags: w3ctrace.Flags{
				Sampled: !sc.Suppressed,
			},
		})
	}

	p := trCtx.Parent()
	p.ParentID = spanID

	trCtx.RawParent = p.String()
	trCtx.RawState = trCtx.State().Add(w3ctrace.VendorInstana, traceID+";"+spanID).String()

	w3ctrace.Inject(trCtx, h)
}

func extractTraceContext(opaqueCarrier interface{}) (SpanContext, error) {
	spanContext := SpanContext{
		Baggage: make(map[string]string),
	}

	carrier, ok := opaqueCarrier.(ot.TextMapReader)
	if !ok {
		return spanContext, ot.ErrInvalidCarrier
	}

	var fieldCount int
	err := carrier.ForeachKey(func(k, v string) error {
		switch strings.ToLower(k) {
		case FieldT:
			fieldCount++

			traceID, err := ParseID(v)
			if err != nil {
				return ot.ErrSpanContextCorrupted
			}

			spanContext.TraceID = traceID
		case FieldS:
			fieldCount++

			spanID, err := ParseID(v)
			if err != nil {
				return ot.ErrSpanContextCorrupted
			}

			spanContext.SpanID = spanID
		case FieldL:
			spanContext.Suppressed = parseLevel(v)
		default:
			if strings.HasPrefix(strings.ToLower(k), FieldB) {
				// preserve original case of the baggage key
				spanContext.Baggage[k[len(FieldB):]] = v
			}
		}

		return nil
	})
	if err != nil {
		return spanContext, err
	}

	if fieldCount == 0 {
		return spanContext, ot.ErrSpanContextNotFound
	} else if fieldCount < 2 {
		return spanContext, ot.ErrSpanContextCorrupted
	}

	if c, ok := opaqueCarrier.(ot.HTTPHeadersCarrier); ok {
		if trCtx, err := w3ctrace.Extract(http.Header(c)); err == nil {
			spanContext.ForeignParent = trCtx
		}
	}

	return spanContext, nil
}

func parseLevel(s string) bool {
	return s == "0"
}

func formatLevel(sc SpanContext) string {
	if sc.Suppressed {
		return "0"
	}

	return "1"
}
