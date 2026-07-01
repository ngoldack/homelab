## ADDED Requirements

### Requirement: Workflow automation via n8n
The system SHALL run n8n with its own Postgres and Valkey, able to call the model layer and other in-cluster services.

#### Scenario: Workflow calls the model layer
- **WHEN** an n8n workflow invokes an LLM step
- **THEN** the call goes to agentgateway and returns a completion from llama.cpp
