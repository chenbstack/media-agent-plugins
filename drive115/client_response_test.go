package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestErrno115AcceptsNumberAndString(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want errno115
	}{
		{name: "number", raw: `{"errno":20004}`, want: 20004},
		{name: "string", raw: `{"errno":"20004"}`, want: 20004},
		{name: "empty string", raw: `{"errno":""}`, want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var response struct {
				ErrNo errno115 `json:"errno"`
			}
			if err := json.Unmarshal([]byte(test.raw), &response); err != nil {
				t.Fatal(err)
			}
			if response.ErrNo != test.want {
				t.Fatalf("errno=%d, want %d", response.ErrNo, test.want)
			}
		})
	}
}

func TestDoJSONWithUserAgent115(t *testing.T) {
	want := "MediaPlayer/1.0"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.UserAgent(); got != want {
			t.Errorf("User-Agent = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":true}`))
	}))
	defer server.Close()

	client := newClient115("", server.Client(), nil)
	var response simple115Response
	if err := client.doJSONWithUserAgent(context.Background(), http.MethodGet, server.URL, nil, nil, &response, want); err != nil {
		t.Fatal(err)
	}
}
