package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kam.bytecoder.nl/internal/kiss"
)

func TestRunReturnsManagerLinkFailure(t *testing.T) {
	manager := &kiss.Manager{}
	manager.TestInit()
	manager.Fail(errors.New("serial disconnected"))
	node := &Node{manager: manager, configPath: t.TempDir() + "/kam.yml"}

	err := node.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "serial disconnected") {
		t.Fatalf("Run error = %v, want serial link failure", err)
	}
}
