package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/mbianchidev/porto/internal/dataops"
	portodocker "github.com/mbianchidev/porto/internal/docker"
)

func TestMigrationCLIResumeRetainsSavedIdentitiesAndRequiresFreshConsent(t *testing.T) {
	saved := dataops.Selection{Kind: "volume", Name: "synthetic-source", ID: "original-volume-id", Destination: "synthetic-restored"}
	submitted := false
	mockDataAPI(t, func(request *http.Request) (any, error) {
		switch request.URL.Path {
		case "/api/data/operations/9":
			return dataops.Operation{ID: 9, Status: "interrupted", Request: dataops.Request{
				Action: "migration", Context: "synthetic-context", Selections: []dataops.Selection{saved},
				IncludeSensitive: true, AllowSourceHelper: true, Confirm: true, Preview: "expired-preview",
			}}, nil
		case "/api/docker/storage/preview":
			var body dataops.Request
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				return nil, err
			}
			if body.Context != "synthetic-context" || len(body.Selections) != 1 || body.Selections[0] != saved ||
				body.IncludeSensitive || body.AllowSourceHelper || body.Confirm || body.Preview != "" {
				return nil, fmt.Errorf("resume widened a saved identity or reused consent: %+v", body)
			}
			body.Preview = "fresh-preview"
			return portodocker.MigrationPreview{Request: body, Token: body.Preview}, nil
		case "/api/data/operations":
			var body dataops.Request
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				return nil, err
			}
			if !body.Confirm || body.Preview != "fresh-preview" || len(body.Selections) != 1 || body.Selections[0] != saved {
				return nil, fmt.Errorf("CLI did not confirm its new identity-bound preview: %+v", body)
			}
			submitted = true
			return dataops.Operation{ID: 10}, nil
		default:
			return nil, fmt.Errorf("resume must not resolve saved selectors against new names: %s", request.URL.Path)
		}
	})
	if err := dockerMigrationCmd([]string{"--resume", "9", "--confirm"}); err != nil {
		t.Fatal(err)
	}
	if !submitted {
		t.Fatal("the fresh resume preview was not submitted")
	}
}

func TestMigrationCLIRenamesOnlyExplicitSelectedDestinations(t *testing.T) {
	mockDataAPI(t, func(request *http.Request) (any, error) {
		switch request.URL.Path {
		case "/api/docker/migration/inventory":
			return portodocker.MigrationInventory{Objects: []portodocker.MigrationObject{
				{Kind: "volume", Name: "synthetic-source", ID: "volume-1", Supported: true},
			}}, nil
		case "/api/docker/storage/preview":
			var body dataops.Request
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				return nil, err
			}
			if len(body.Selections) != 1 || body.Selections[0].Destination != "synthetic-renamed" || body.Selections[0].ID != "volume-1" {
				return nil, fmt.Errorf("destination override lost its original selection: %+v", body)
			}
			return portodocker.MigrationPreview{Request: body}, nil
		default:
			return nil, fmt.Errorf("unexpected request: %s", request.URL.Path)
		}
	})
	if err := dockerMigrationCmd([]string{"--context", "synthetic-context", "--objects", "volume:synthetic-source", "--destinations", "volume:synthetic-source=synthetic-renamed"}); err != nil {
		t.Fatal(err)
	}
}
