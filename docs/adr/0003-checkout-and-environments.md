# 0003. Embedded checkout and explicit environments

**Status** Accepted.

The host website owns checkout routes, styling, pricing, ownership checks, and customer-facing completion. Use provider-approved fields and components, with an origin allowlist and Content Security Policy. Map provider responses to typed, expiring customer actions and apply host policy before exposing them to a browser. Only documented in-page components, frames, and approved in-page modals satisfy the default policy. Disable paths that require top-level navigation, and require product approval before enabling any separate popup. Preserve mandatory authentication.

Define explicit `MOCK`, `SANDBOX`, and `LIVE` environments. Restrict mock traffic and synthetic callbacks to approved local endpoints. Use official sandbox hosts and test credentials in sandbox. Require merchant activation and approved endpoints for live configuration. Keep data and credential boundaries distinct, restrict redirects, and verify provider callbacks before inbox writes.

Protocol-level simulators mount as handlers in a consumer-owned HTTP server and act like external providers to the adapters. An in-process provider double remains useful for unit tests, while the simulators exercise actual wire encoding, authentication, and callback delivery. Sandbox browser and server evidence establish the tested external behavior.
