## ADDED Requirements

### Requirement: Central model alias layer
The system SHALL provide LiteLLM as the single OpenAI-compatible entry point in front of llama.cpp, exposing stable aliases so consumers never reference a concrete model.

#### Scenario: Alias resolves to llama.cpp
- **WHEN** a client calls LiteLLM with model `default`
- **THEN** the request is served by the llama.cpp backend
- **AND** changing the backend model requires editing only the LiteLLM config

#### Scenario: Reuse, no model redeploy
- **WHEN** LiteLLM is added
- **THEN** it points at the existing v0 llama.cpp Service and no new model server is deployed
