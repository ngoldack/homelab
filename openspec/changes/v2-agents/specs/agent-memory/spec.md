## ADDED Requirements

### Requirement: Semantic agent memory via mem0
The system SHALL run mem0 on its own pgvector instance, using the LiteLLM `embeddings` alias for embeddings, exposed to kagent as an MCP memory server.

#### Scenario: Store and recall
- **WHEN** an agent stores a memory and later queries for it
- **THEN** mem0 embeds via LiteLLM, persists to pgvector, and returns the relevant memory on recall

#### Scenario: Reuses the model layer
- **WHEN** mem0 needs embeddings or extraction
- **THEN** it calls LiteLLM aliases, not a separate model server
