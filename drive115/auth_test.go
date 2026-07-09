package main

import (
	"encoding/json"
	"testing"
)

func TestCookieHeaderFromAny115(t *testing.T) {
	got := cookieHeaderFromAny115(map[string]any{
		"SEID": "seid-value",
		"UID":  "uid-value",
		"CID":  "cid-value",
	})
	want := "CID=cid-value; SEID=seid-value; UID=uid-value"
	if got != want {
		t.Fatalf("cookieHeaderFromAny115() = %q, want %q", got, want)
	}
}

func TestInt64Value115(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  int64
		ok    bool
	}{
		{name: "json number", value: json.Number("-2"), want: -2, ok: true},
		{name: "string", value: "2", want: 2, ok: true},
		{name: "empty string", value: "", ok: false},
		{name: "nil", value: nil, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := int64Value115(tt.value)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("int64Value115() = (%d, %v), want (%d, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}
