package embeddings

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestOpenAIClientRequestsPresetAndOrdersVectors(t *testing.T) {
	client := newOpenAIClient("private-key")
	client.endpoint = "https://openai.invalid/v1/embeddings"
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Authorization"); got != "Bearer private-key" {
			t.Fatalf("authorization = %q", got)
		}
		var body embeddingRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != ModelSmall || body.Dimensions != Dimensions || body.EncodingFormat != "float" {
			t.Fatalf("request = %+v", body)
		}
		if len(body.Input) != 2 || body.Input[0] != "first" || body.Input[1] != "second" {
			t.Fatalf("input = %#v", body.Input)
		}
		response := embeddingResponse{}
		response.Data = make([]struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}, 2)
		response.Data[0].Index = 1
		response.Data[0].Embedding = testVector(2)
		response.Data[1].Index = 0
		response.Data[1].Embedding = testVector(1)
		encoded, _ := json.Marshal(response)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
	})}

	vectors, err := client.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatal(err)
	}
	if vectors[0][0] != 1 || vectors[1][0] != 2 {
		t.Fatalf("vectors returned out of input order: %v %v", vectors[0][0], vectors[1][0])
	}
}

func TestOpenAIClientDoesNotPersistProviderBodyInError(t *testing.T) {
	client := newOpenAIClient("private-key")
	client.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader(`{"error":"echoed private message"}`)),
		}, nil
	})}
	_, err := client.Embed(context.Background(), []string{"private message"})
	if err == nil || strings.Contains(err.Error(), "private message") {
		t.Fatalf("unsafe error = %v", err)
	}
}

func testVector(first float32) []float32 {
	vector := make([]float32, Dimensions)
	vector[0] = first
	return vector
}

func TestOpenAIClientSeparatesRejectedInputFromRetryableFailures(t *testing.T) {
	client := newOpenAIClient("private-key")
	client.endpoint = "https://openai.invalid/v1/embeddings"
	client.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	if _, err := client.Embed(context.Background(), []string{"too many tokens"}); !errors.Is(err, ErrRejectedInput) {
		t.Fatalf("rejected input error = %v", err)
	}
	// A non-breaking space is blank to the provider and never reaches the wire.
	if _, err := client.Embed(context.Background(), []string{string(rune(0x00A0))}); !errors.Is(err, ErrRejectedInput) {
		t.Fatalf("blank input error = %v", err)
	}

	// Rate limits and server faults are worth retrying and must not be
	// mistaken for input the provider will never accept.
	client.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	_, err := client.Embed(context.Background(), []string{"retry me"})
	if err == nil || errors.Is(err, ErrRejectedInput) {
		t.Fatalf("rate limited error = %v", err)
	}
}
