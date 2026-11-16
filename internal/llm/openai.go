package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

// OpenAI talks to the OpenAI Responses API, the same API and model setting
// the original single-file implementation used.
type OpenAI struct {
	client openai.Client
	model  string
	// noCompaction is set once the endpoint rejects context_management (some
	// OpenAI-compatible servers do), so later requests stop sending it.
	noCompaction atomic.Bool
	// noToolSearch is set once the endpoint rejects deferred function loading.
	noToolSearch atomic.Bool
}

// NewOpenAI builds the provider. baseURL may be empty (SDK default / OPENAI_BASE_URL).
func NewOpenAI(apiKey, baseURL, model string) (*OpenAI, error) {
	if apiKey == "" {
		return nil, &Error{Kind: ErrConfig, Err: errors.New("OPENAI_API_KEY is not set")}
	}
	if model == "" {
		return nil, &Error{Kind: ErrConfig, Err: errors.New("no model configured")}
	}
	opts := []option.RequestOption{option.WithAPIKey(apiKey), option.WithMaxRetries(0)} // retries are handled by WithRetry
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &OpenAI{client: openai.NewClient(opts...), model: model}, nil
}

func (o *OpenAI) Name() string  { return "openai" }
func (o *OpenAI) Model() string { return o.model }

func (o *OpenAI) Generate(ctx context.Context, req Request) (*Response, error) {
	compact := req.CompactAt > 0 && !o.noCompaction.Load()
	deferTools := req.DeferTools && len(req.Tools) > 0 && supportsToolSearch(o.model) && !o.noToolSearch.Load()
	for attempts := 0; attempts < 3; attempts++ {
		out, err := o.generate(ctx, req, compact, deferTools)
		if err == nil {
			return out, nil
		}
		switch {
		case compact && rejectsCompaction(err):
			compact = false
			o.noCompaction.Store(true)
		case deferTools && rejectsToolSearch(err):
			deferTools = false
			o.noToolSearch.Store(true)
		default:
			return nil, err
		}
	}
	return nil, &Error{Kind: ErrBadRequest, Err: errors.New("provider rejected optional request features")}
}

func supportsToolSearch(model string) bool {
	name := strings.ToLower(strings.TrimPrefix(model, "gpt-"))
	parts := strings.SplitN(name, ".", 2)
	if len(parts) != 2 {
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minorPart := strings.SplitN(parts[1], "-", 2)[0]
	minor, err := strconv.Atoi(minorPart)
	if err != nil {
		return false
	}
	return major > 5 || (major == 5 && minor >= 4)
}

// CompactionSupported reports whether the endpoint has accepted (or not yet
// refused) context_management.
func (o *OpenAI) CompactionSupported() bool { return !o.noCompaction.Load() }

// rejectsCompaction recognises a 400 caused by the context_management field.
func rejectsCompaction(err error) bool {
	var le *Error
	if !errors.As(err, &le) || le.Kind != ErrBadRequest {
		return false
	}
	msg := strings.ToLower(le.Err.Error())
	return strings.Contains(msg, "context_management") || strings.Contains(msg, "compact")
}

func rejectsToolSearch(err error) bool {
	var le *Error
	if !errors.As(err, &le) || le.Kind != ErrBadRequest {
		return false
	}
	msg := strings.ToLower(le.Err.Error())
	return strings.Contains(msg, "tool_search") || strings.Contains(msg, "tool search") || strings.Contains(msg, "defer_loading")
}

func (o *OpenAI) generate(ctx context.Context, req Request, compact, deferTools bool) (*Response, error) {
	params := responses.ResponseNewParams{
		Model:        openai.ChatModel(o.model),
		Instructions: openai.String(req.Instructions),
		Truncation:   responses.ResponseNewParamsTruncationAuto, // let the server drop the oldest turns instead of failing on overflow
	}
	var items responses.ResponseInputParam
	for _, t := range req.ToolOutputs {
		it := responses.ResponseInputItemParamOfFunctionCallOutput(t.Output)
		it.OfFunctionCallOutput.CallID = openai.String(t.CallID)
		items = append(items, it)
	}
	if req.UserText != "" {
		items = append(items, responses.ResponseInputItemParamOfMessage(req.UserText, responses.EasyInputMessageRoleUser))
	}
	params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: items}
	if compact {
		params.ContextManagement = []responses.ResponseNewParamsContextManagement{{Type: "compaction", CompactThreshold: openai.Int(req.CompactAt)}}
	}
	if req.PreviousID != "" {
		params.PreviousResponseID = openai.String(req.PreviousID)
	}
	if deferTools {
		params.Tools = append(params.Tools, responses.ToolUnionParam{OfToolSearch: &responses.ToolSearchToolParam{
			Type: "tool_search", Execution: responses.ToolSearchToolExecutionServer,
		}})
	}
	for _, t := range req.Tools {
		var schema map[string]any
		if err := json.Unmarshal(t.Schema, &schema); err != nil {
			return nil, &Error{Kind: ErrConfig, Err: errors.New("tool " + t.Name + " has an invalid schema: " + err.Error())}
		}
		tp := responses.ToolParamOfFunction(t.Name, schema, false)
		tp.OfFunction.Description = openai.String(t.Description)
		if deferTools {
			tp.OfFunction.DeferLoading = openai.Bool(true)
		}
		params.Tools = append(params.Tools, tp)
	}

	resp, err := o.stream(ctx, params, req.OnDelta)
	if err != nil {
		return nil, classify(ctx, err)
	}
	out := &Response{
		ID:   resp.ID,
		Text: strings.TrimSpace(resp.OutputText()),
		Usage: Usage{
			InputTokens: resp.Usage.InputTokens, CachedInputTokens: resp.Usage.InputTokensDetails.CachedTokens,
			OutputTokens: resp.Usage.OutputTokens,
		},
	}
	for _, item := range resp.Output {
		switch item.Type {
		case "function_call":
			fc := item.AsFunctionCall()
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: fc.CallID, Name: fc.Name, Arguments: fc.Arguments})
		case "compaction":
			out.Compacted = true
		}
	}
	if resp.Status == responses.ResponseStatusIncomplete {
		out.Incomplete = string(resp.IncompleteDetails.Reason)
		if out.Incomplete == "" {
			out.Incomplete = "incomplete"
		}
	}
	if resp.Status == responses.ResponseStatusFailed {
		return nil, &Error{Kind: ErrTransient, Err: errors.New("model response failed: " + resp.Error.Message)}
	}
	return out, nil
}

// stream runs the request through the Responses streaming API, forwarding
// output-text deltas to onDelta as they arrive, and returns the final response
// once the stream completes.
func (o *OpenAI) stream(ctx context.Context, params responses.ResponseNewParams, onDelta func(string)) (*responses.Response, error) {
	s := o.client.Responses.NewStreaming(ctx, params)
	defer s.Close()
	var final *responses.Response
	for s.Next() {
		ev := s.Current()
		switch ev.Type {
		case "response.output_text.delta":
			if onDelta != nil && ev.Delta != "" {
				onDelta(ev.Delta)
			}
		case "response.completed", "response.failed", "response.incomplete":
			final = &ev.Response
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if final == nil {
		return nil, errors.New("model stream ended without a final response")
	}
	return final, nil
}

func classify(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return &Error{Kind: ErrCancelled, Err: ctx.Err()}
	}
	var api *openai.Error
	if errors.As(err, &api) {
		e := &Error{Status: api.StatusCode, Err: errors.New(api.Message)}
		if api.Message == "" {
			e.Err = errors.New(http.StatusText(api.StatusCode))
		}
		switch {
		case api.StatusCode == 401 || api.StatusCode == 403:
			e.Kind = ErrAuth
		case api.StatusCode == 429:
			e.Kind = ErrRateLimit
			if api.Response != nil {
				e.RetryAfter = parseRetryAfter(api.Response.Header.Get("Retry-After"))
			}
		case api.StatusCode >= 500 || api.StatusCode == 408 || api.StatusCode == 409:
			e.Kind = ErrTransient
		case api.Code == "context_length_exceeded":
			e.Kind = ErrContext
		default:
			e.Kind = ErrBadRequest
		}
		return e
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) {
		return &Error{Kind: ErrTransient, Err: err}
	}
	return &Error{Kind: ErrTransient, Err: err} // connection resets, DNS failures, etc.
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
