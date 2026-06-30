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

### Requirement: Dedicated CPU inference for a large coder model
The system SHALL serve a large coder model (`qwen3-coder-next`) via llama.cpp on CPU, on a dedicated CPU-only node (72GB), exposed OpenAI-compatible in-cluster under the `coder` alias.

#### Scenario: Coder model runs only on the CPU node
- **WHEN** the CPU llama.cpp pod is scheduled
- **THEN** it lands only on the `workload=cpu-inference` node (nodeSelector + toleration of its taint)
- **AND** it serves completions on CPU (`-ngl 0`) with the model mlock'd into RAM

#### Scenario: Coder model is reachable
- **WHEN** a client POSTs to `/v1/chat/completions` on the CPU llama.cpp Service
- **THEN** it receives a completion from the coder model
