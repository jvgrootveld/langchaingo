// package googleai implements a langchaingo provider for Google AI LLMs.
// See https://ai.google.dev/ for more details and documetnation.
//
//nolint:goerr113, lll
package googleai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/google/generative-ai-go/genai"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// GoogleAI is a type that represents a Google AI API client.
type GoogleAI struct {
	client *genai.Client
	opts   options
}

var (
	_ llms.Model = &GoogleAI{}

	ErrNoContentInResponse    = errors.New("no content in generation response")
	ErrUnknownPartInResponse  = errors.New("unknown part type in generation response")
	ErrInvalidMimeType        = errors.New("invalid mime type on content")
	ErrSystemRoleNotSupported = errors.New("system role isn't supporeted yet")
)

const (
	CITATIONS = "citations"
	SAFETY    = "safety"
	RoleModel = "model"
	RoleUser  = "user"
)

// NewGoogleAI creates a new GoogleAI struct.
func NewGoogleAI(ctx context.Context, opts ...Option) (*GoogleAI, error) {
	clientOptions := defaultOptions()
	for _, opt := range opts {
		opt(&clientOptions)
	}

	gi := &GoogleAI{
		opts: clientOptions,
	}

	client, err := genai.NewClient(ctx, option.WithAPIKey(clientOptions.apiKey))
	if err != nil {
		return gi, err
	}

	gi.client = client
	return gi, nil
}

// GenerateContent calls the LLM with the provided parts.
func (g *GoogleAI) GenerateContent(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	opts := llms.CallOptions{
		Model:       g.opts.defaultModel,
		MaxTokens:   int(g.opts.defaultMaxTokens),
		Temperature: float64(g.opts.defaultTemperature),
	}
	for _, opt := range options {
		opt(&opts)
	}

	model := g.client.GenerativeModel(opts.Model)
	model.SetMaxOutputTokens(int32(opts.MaxTokens))
	model.SetTemperature(float32(opts.Temperature))

	if len(messages) == 1 {
		theMessage := messages[0]
		if theMessage.Role != schema.ChatMessageTypeHuman {
			return nil, fmt.Errorf("got %v message role, want human", theMessage.Role)
		}
		return generateFromSingleMessage(ctx, model, theMessage.Parts, &opts)
	}
	return generateFromMessages(ctx, model, messages, &opts)
}

// downloadImageData downloads the content from the given URL and returns it as
// a *genai.Blob.
func downloadImageData(url string) (*genai.Blob, error) {
	resp, err := http.Get(url) //nolint
	if err != nil {
		return nil, fmt.Errorf("failed to fetch image from url: %w", err)
	}
	defer resp.Body.Close()

	urlData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read image bytes: %w", err)
	}

	mimeType := resp.Header.Get("Content-Type")

	// The convenience function genai.ImageData requires just the right part of
	// the mime type, so we need to parse it
	parts := strings.Split(mimeType, "/")

	if len(parts) != 2 { //nolint
		return nil, ErrInvalidMimeType
	}

	blob := genai.ImageData(parts[1], urlData)

	return &blob, nil
}

// convertCandidates converts a sequence of genai.Candidate to a response.
func convertCandidates(candidates []*genai.Candidate) (*llms.ContentResponse, error) {
	var contentResponse llms.ContentResponse

	for _, candidate := range candidates {
		buf := strings.Builder{}

		for _, part := range candidate.Content.Parts {
			if v, ok := part.(genai.Text); ok {
				_, err := buf.WriteString(string(v))
				if err != nil {
					return nil, err
				}
			} else {
				return nil, ErrUnknownPartInResponse
			}
		}

		metadata := make(map[string]any)
		metadata[CITATIONS] = candidate.CitationMetadata
		metadata[SAFETY] = candidate.SafetyRatings

		contentResponse.Choices = append(contentResponse.Choices,
			&llms.ContentChoice{
				Content:        buf.String(),
				StopReason:     candidate.FinishReason.String(),
				GenerationInfo: metadata,
			})
	}
	return &contentResponse, nil
}

// CreateEmbedding creates embeddings from texts.
func (g *GoogleAI) CreateEmbedding(ctx context.Context, texts []string) ([][]float32, error) {
	em := g.client.EmbeddingModel(g.opts.defaultEmbeddingModel)

	results := make([][]float32, 0, len(texts))
	for _, t := range texts {
		res, err := em.EmbedContent(ctx, genai.Text(t))
		if err != nil {
			return results, err
		}
		results = append(results, res.Embedding.Values)
	}

	return results, nil
}

// convertParts converts between a sequence of langchain parts and genai parts.
func convertParts(parts []llms.ContentPart) ([]genai.Part, error) {
	convertedParts := make([]genai.Part, 0, len(parts))
	for _, part := range parts {
		var out genai.Part
		var err error

		switch p := part.(type) {
		case llms.TextContent:
			out = genai.Text(p.Text)
		case llms.BinaryContent:
			out = genai.Blob{MIMEType: p.MIMEType, Data: p.Data}
		case llms.ImageURLContent:
			out, err = downloadImageData(p.URL)
		}
		if err != nil {
			return nil, err
		}

		convertedParts = append(convertedParts, out)
	}
	return convertedParts, nil
}

// convertContent converts between a langchain MessageContent and genai content.
func convertContent(content llms.MessageContent) (*genai.Content, error) {
	parts, err := convertParts(content.Parts)
	if err != nil {
		return nil, err
	}

	c := &genai.Content{
		Parts: parts,
	}

	switch content.Role {
	case schema.ChatMessageTypeSystem:
		return nil, ErrSystemRoleNotSupported
	case schema.ChatMessageTypeAI:
		c.Role = RoleModel
	case schema.ChatMessageTypeHuman:
		c.Role = RoleUser
	case schema.ChatMessageTypeGeneric:
		c.Role = RoleUser
	case schema.ChatMessageTypeFunction:
		fallthrough
	default:
		return nil, fmt.Errorf("role %v not supported", content.Role)
	}

	return c, nil
}

// generateFromSingleMessage generates content from the parts of a single
// message.
func generateFromSingleMessage(ctx context.Context, model *genai.GenerativeModel, parts []llms.ContentPart, opts *llms.CallOptions) (*llms.ContentResponse, error) {
	convertedParts, err := convertParts(parts)
	if err != nil {
		return nil, err
	}

	if opts.StreamingFunc == nil {
		// When no streaming is requested, just call GenerateContent and return
		// the complete response with a list of candidates.
		resp, err := model.GenerateContent(ctx, convertedParts...)
		if err != nil {
			return nil, err
		}

		if len(resp.Candidates) == 0 {
			return nil, ErrNoContentInResponse
		}
		return convertCandidates(resp.Candidates)
	}
	iter := model.GenerateContentStream(ctx, convertedParts...)
	return convertAndStreamFromIterator(ctx, iter, opts)
}

func generateFromMessages(ctx context.Context, model *genai.GenerativeModel, messages []llms.MessageContent, opts *llms.CallOptions) (*llms.ContentResponse, error) {
	history := make([]*genai.Content, 0, len(messages))
	for _, mc := range messages {
		content, err := convertContent(mc)
		if err != nil {
			return nil, err
		}
		history = append(history, content)
	}

	// Given N total messages, genai's chat expects the first N-1 messages as
	// history and the last message as the actual request.
	n := len(history)
	reqContent := history[n-1]
	history = history[:n-1]

	if reqContent.Role != RoleUser {
		return nil, fmt.Errorf("got %v message role, want user/human", reqContent.Role)
	}

	session := model.StartChat()
	session.History = history

	if opts.StreamingFunc == nil {
		resp, err := session.SendMessage(ctx, reqContent.Parts...)
		if err != nil {
			return nil, err
		}

		if len(resp.Candidates) == 0 {
			return nil, ErrNoContentInResponse
		}
		return convertCandidates(resp.Candidates)
	}
	iter := session.SendMessageStream(ctx, reqContent.Parts...)
	return convertAndStreamFromIterator(ctx, iter, opts)
}

// convertAndStreamFromIterator takes an iterator of GenerateContentResponse
// and produces a llms.ContentResponse reply from it, while streaming the
// resulting text into the opts-provided streaming function.
// Note that this is tricky in the face of multiple
// candidates, so this code assumes only a single candidate for now.
func convertAndStreamFromIterator(ctx context.Context, iter *genai.GenerateContentResponseIterator, opts *llms.CallOptions) (*llms.ContentResponse, error) {
	candidate := &genai.Candidate{
		Content: &genai.Content{},
	}
DoStream:
	for {
		resp, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			log.Fatal(err)
		}

		if len(resp.Candidates) != 1 {
			return nil, fmt.Errorf("expect single candidate in stream mode; got %v", len(resp.Candidates))
		}
		respCandidate := resp.Candidates[0]
		candidate.Content.Parts = append(candidate.Content.Parts, respCandidate.Content.Parts...)
		candidate.Content.Role = respCandidate.Content.Role
		candidate.FinishReason = respCandidate.FinishReason
		candidate.SafetyRatings = respCandidate.SafetyRatings
		candidate.CitationMetadata = respCandidate.CitationMetadata
		candidate.TokenCount += respCandidate.TokenCount

		for _, part := range respCandidate.Content.Parts {
			if text, ok := part.(genai.Text); ok {
				if opts.StreamingFunc(ctx, []byte(text)) != nil {
					break DoStream
				}
			}
		}
	}

	return convertCandidates([]*genai.Candidate{candidate})
}
