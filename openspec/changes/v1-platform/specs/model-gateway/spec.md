## ADDED Requirements

### Requirement: Central model gateway
The system SHALL provide agentgateway as the single OpenAI-compatible entry point in front of the llama.cpp InferenceServices, exposing stable model names so consumers never reference a concrete backend, with per-call OpenTelemetry traces + Prometheus metrics emitted to the observability stack.

#### Scenario: Model name resolves to llama.cpp
- **WHEN** a client calls agentgateway with a stable model name (e.g. `default`)
- **THEN** the request is routed to the corresponding llama.cpp InferenceService backend
- **AND** changing the backend requires editing only the agentgateway route config

#### Scenario: Reuse, no model redeploy
- **WHEN** agentgateway is added
- **THEN** it points at the existing llama.cpp InferenceServices and no new model server is deployed

#### Scenario: LLM traffic is observable
- **WHEN** a request passes through agentgateway
- **THEN** a trace is exported (via the OTel collector) to VictoriaTraces and request/token metrics are scraped into VictoriaMetrics
