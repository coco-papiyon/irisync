package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGitHubMutationResult(t *testing.T) {
	const requestID = "ABCD:1234:DEADBEEF:12345678"
	tests := []struct {
		name       string
		endpoint   string
		output     string
		commandErr error
		want       string
	}{
		{
			name:       "REST gateway error with request ID",
			endpoint:   "repos/example/project/issues/31/comments",
			output:     "HTTP/2.0 502 Bad Gateway\r\nX-GitHub-Request-Id: " + requestID + "\r\nSet-Cookie: sensitive-header\r\n\r\nsensitive-body",
			commandErr: errors.New("exit status 1"),
			want:       `GitHub POST "repos/example/project/issues/31/comments": HTTP 502 (request ID ` + requestID + "): request failed",
		},
		{
			name:     "decoded GraphQL success",
			endpoint: "graphql",
			output:   "HTTP/2.0 200 OK\nContent-Encoding: gzip\n\n" + `{"data":{"updateProjectV2ItemFieldValue":{"clientMutationId":null}}}`,
		},
		{
			name:       "GraphQL rejection with HTTP success",
			endpoint:   "graphql",
			output:     "HTTP/2.0 200 OK\nX-GitHub-Request-Id: sensitive-header\n\n" + `{"errors":[{"message":"sensitive-body"}]}`,
			commandErr: errors.New("exit status 1"),
			want:       `GitHub POST "graphql": HTTP 200: GraphQL mutation rejected`,
		},
		{
			name:       "timeout before HTTP response",
			endpoint:   "graphql",
			output:     "sensitive-body",
			commandErr: context.DeadlineExceeded,
			want:       `GitHub POST "graphql": no HTTP response: context deadline exceeded`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := githubMutationResult("POST", tt.endpoint, []byte(tt.output), tt.commandErr)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if tt.commandErr == context.DeadlineExceeded && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("error does not wrap the context deadline")
			}
			if strings.Contains(err.Error(), "sensitive-") {
				t.Fatal("error includes response content")
			}
		})
	}
}
