package discovery

import (
	"errors"
	"net"
	"testing"
)

func TestDBDownClassification(t *testing.T) {
	if !isDBDown(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}) {
		t.Fatal("network dial failure must be DB_DOWN")
	}
	if isDBDown(errors.New("access denied for user")) {
		t.Fatal("credentials error must be indeterminate")
	}
	if isDBDown(errors.New("SHOW REPLICA STATUS access denied")) {
		t.Fatal("replication privilege failure must be indeterminate")
	}
}
