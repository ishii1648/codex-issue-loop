package main

import (
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/delivery"
	"github.com/ishii1648/codex-issue-loop/internal/schema"
	"github.com/ishii1648/codex-issue-loop/internal/statecontract"
)

func TestReleaseContractConstantsStayAligned(t *testing.T) {
	if delivery.ProtocolVersion != 1 || schema.Current != statecontract.CurrentSchemaVersion || schema.Previous != statecontract.MigrationFromSchema {
		t.Fatalf("protocol/schema contract drift: protocol=%d schema=%d/%d contract=%d/%d", delivery.ProtocolVersion, schema.Current, schema.Previous, statecontract.CurrentSchemaVersion, statecontract.MigrationFromSchema)
	}
}
