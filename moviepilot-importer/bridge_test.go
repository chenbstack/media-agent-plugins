package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMoviePilotClientLogsInAndPaginatesStandardAPIs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/login/access-token" && r.Header.Get("Authorization") != "Bearer mp-token" {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/login/access-token":
			if err := r.ParseForm(); err != nil || r.Form.Get("username") != "admin" || r.Form.Get("password") != "secret" {
				http.Error(w, "bad credentials", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"mp-token","super_user":true,"user_name":"admin"}`)
		case "/api/v1/system/global":
			if r.URL.Query().Get("token") != "moviepilot" {
				http.Error(w, "missing global token", http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, `{"success":true,"data":{"BACKEND_VERSION":"2.9.11"}}`)
		case "/api/v1/site/":
			_, _ = io.WriteString(w, `[{"id":1,"name":"Site A"},{"id":2,"name":"Site B"}]`)
		case "/api/v1/history/transfer":
			if r.URL.Query().Get("page") == "1" {
				_, _ = io.WriteString(w, `{"success":true,"data":{"list":[{"id":10,"title":"A"}],"total":2}}`)
			} else {
				_, _ = io.WriteString(w, `{"success":true,"data":{"list":[{"id":11,"title":"B"}],"total":2}}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newMoviePilotClient(config{BaseURL: server.URL + "/api/v1", Username: "admin", Password: "secret"})
	ping, err := client.ping(context.Background())
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if ping.MoviePilot["version"] != "2.9.11" || ping.MoviePilot["username"] != "admin" {
		t.Fatalf("ping result = %#v", ping)
	}

	sites, err := client.export(context.Background(), "sites", "", 1)
	if err != nil {
		t.Fatalf("export sites: %v", err)
	}
	if sites.Done || sites.NextCursor != "1" || len(sites.Items) != 1 || sites.Items[0].SourceID != "1" {
		t.Fatalf("sites page = %#v", sites)
	}
	sites, err = client.export(context.Background(), "sites", sites.NextCursor, 1)
	if err != nil || !sites.Done || len(sites.Items) != 1 || sites.Items[0].SourceID != "2" {
		t.Fatalf("sites last page = %#v, %v", sites, err)
	}

	transfers, err := client.export(context.Background(), "transfer_history", "", 1)
	if err != nil || transfers.Done || transfers.NextCursor != "2" || transfers.Total != 2 {
		t.Fatalf("transfer page = %#v, %v", transfers, err)
	}
	transfers, err = client.export(context.Background(), "transfer_history", transfers.NextCursor, 1)
	if err != nil || !transfers.Done || transfers.Items[0].SourceID != "11" {
		t.Fatalf("transfer last page = %#v, %v", transfers, err)
	}
}

func TestMoviePilotClientRejectsNonAdminAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"mp-token","super_user":false,"user_name":"reader"}`)
	}))
	defer server.Close()

	client := newMoviePilotClient(config{BaseURL: server.URL, Username: "reader", Password: "secret"})
	if _, err := client.ping(context.Background()); err == nil {
		t.Fatal("expected non-admin login to be rejected")
	}
}
