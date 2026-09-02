package codexissueloop

import _ "embed"

//go:embed skill/agent-loop/SKILL.md
var AgentLoopSkill []byte

//go:embed analysis/incident-taxonomy/rules.json
var IncidentRules []byte
