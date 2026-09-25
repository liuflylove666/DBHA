package discovery

import "testing"

func TestValidateRemoteAllowsPlainTransport(t *testing.T) {
	c := Config{
		ServerURL:  "http://127.0.0.1:8080",
		ServerGRPC: "127.0.0.1:50052",
		TokenFile:  "/tmp/agent.token",
		AgentID:    "agent-1",
		StateFile:  "/tmp/identity.json",
	}
	if err := c.ValidateRemote(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRemoteRejectsUnsupportedScheme(t *testing.T) {
	c := Config{ServerURL: "ftp://127.0.0.1", ServerGRPC: "127.0.0.1:50052", TokenFile: "token", AgentID: "agent", StateFile: "state"}
	if err := c.ValidateRemote(); err == nil {
		t.Fatal("unsupported control transport accepted")
	}
}

func TestValidateRemoteAcceptsPairedEndpointLists(t *testing.T) {
	c := Config{
		ServerURLs:          []string{"http://controller-1:8080", "http://controller-2:8080/"},
		ServerGRPCEndpoints: []string{"controller-1:50052", "controller-2:50052"},
		TokenFile:           "token",
		AgentID:             "agent",
		StateFile:           "state",
	}
	if err := c.ValidateRemote(); err != nil {
		t.Fatal(err)
	}
	urls, grpcEndpoints, err := c.serverEndpoints()
	if err != nil || urls[1] != "http://controller-2:8080" || grpcEndpoints[1] != "controller-2:50052" {
		t.Fatalf("endpoints not normalized: %v %v %v", urls, grpcEndpoints, err)
	}
}

func TestValidateRemoteRejectsUnpairedOrDuplicateEndpointLists(t *testing.T) {
	base := Config{TokenFile: "token", AgentID: "agent", StateFile: "state"}
	for name, c := range map[string]Config{
		"unpaired": {
			ServerURLs:          []string{"http://controller-1:8080", "http://controller-2:8080"},
			ServerGRPCEndpoints: []string{"controller-1:50052"},
		},
		"duplicate HTTP": {
			ServerURLs:          []string{"http://controller-1:8080", "http://controller-1:8080/"},
			ServerGRPCEndpoints: []string{"controller-1:50052", "controller-2:50052"},
		},
		"duplicate gRPC": {
			ServerURLs:          []string{"http://controller-1:8080", "http://controller-2:8080"},
			ServerGRPCEndpoints: []string{"controller-1:50052", "controller-1:50052"},
		},
	} {
		c.TokenFile, c.AgentID, c.StateFile = base.TokenFile, base.AgentID, base.StateFile
		if err := c.ValidateRemote(); err == nil {
			t.Fatalf("%s endpoints accepted", name)
		}
	}
}
