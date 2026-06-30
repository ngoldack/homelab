## ADDED Requirements

### Requirement: OpenAI-compatible LLM inference on the P100
The system SHALL serve `qwen3.5:9b` via llama.cpp (`llama-server`) on the Tesla P100, exposing an OpenAI-compatible API inside the cluster.

#### Scenario: Chat completion succeeds
- **WHEN** a client POSTs to `/v1/chat/completions` on the in-cluster llama.cpp Service
- **THEN** it receives a valid completion generated on the GPU

#### Scenario: Model is fully GPU-offloaded
- **WHEN** the server starts
- **THEN** all model layers are offloaded to the P100 (`-ngl` covers the full model)
- **AND** `nvidia-smi` shows the process resident in GPU memory

### Requirement: Pascal-tuned serving
The llama.cpp deployment SHALL apply Pascal/P100-appropriate optimizations.

#### Scenario: Optimizations applied
- **WHEN** the server is configured
- **THEN** it enables flash-attention, continuous batching with parallel slots, a configured context size, and FP16 compute suited to the P100

### Requirement: Persistent model storage and metrics
Model weights SHALL persist across restarts and the server SHALL expose Prometheus metrics.

#### Scenario: Metrics scraped
- **WHEN** the monitoring stack scrapes the llama.cpp Service
- **THEN** llama.cpp request/throughput metrics appear in VictoriaMetrics

#### Scenario: No re-download on restart
- **WHEN** the pod restarts
- **THEN** weights are read from the PVC without re-downloading
