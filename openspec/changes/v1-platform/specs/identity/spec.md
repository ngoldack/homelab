## ADDED Requirements

### Requirement: Single sign-on via Authentik
The system SHALL provide SSO using Authentik (OIDC) and forward-auth, and externally exposed UIs SHALL require authentication.

#### Scenario: Unauthenticated access is blocked
- **WHEN** an unauthenticated client requests a protected service
- **THEN** it is redirected to Authentik to log in

#### Scenario: OIDC login grants access
- **WHEN** a client completes the Authentik OIDC flow
- **THEN** it is granted access to the requested service
