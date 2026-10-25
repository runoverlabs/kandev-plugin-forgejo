package rest

import (
	"testing"

	"kandev-plugin-forgejo/internal/forgejo/rest/resttest"
)

// apiServer is the scripted instance the client tests run against.
type apiServer = resttest.Server

func newAPIServer(t *testing.T) *apiServer { return resttest.NewServer(t) }
