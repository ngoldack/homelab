## ADDED Requirements

### Requirement: Agent fleet backed by the model layer
The system SHALL run kagent with a ModelConfig that points at the LiteLLM `default` alias, so agents use llama.cpp without referencing a model.

#### Scenario: Agent completes a task
- **WHEN** a kagent agent is invoked
- **THEN** it reasons via LiteLLM → llama.cpp and can complete a tool call

### Requirement: MCP authorization via agentgateway
Agent MCP traffic SHALL pass through agentgateway, which enforces per-agent authentication (JWT from Authentik) and tool authorization.

#### Scenario: Unauthorized MCP call rejected
- **WHEN** an MCP request without a valid agentgateway-accepted token is made
- **THEN** it is rejected

#### Scenario: Authorized MCP call allowed
- **WHEN** an agent presents a valid token and calls a permitted tool
- **THEN** the call is forwarded to the MCP server
