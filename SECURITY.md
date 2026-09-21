# Security

## Reporting a vulnerability

Please report security issues through a [private GitHub security advisory](https://github.com/qaustria/AutoPack-Go/security/advisories/new). Do not include API keys, webhooks, passwords, or exploit details in a public issue.

## Credential handling

- The public web interface sends Roblox credentials only with the active conversion request.
- Cone does not write request credentials to server storage or logs.
- "Remember on this device" uses the browser's local storage and can be turned off.
- Browser local storage is not appropriate on shared or untrusted devices; turn
  remembering off there and remove saved site data afterward.
- Self-hosted CLI credentials belong in the ignored `.env.go` file or environment variables.
- Always deploy Cone behind HTTPS before accepting credentials over a network.
- Administrative batch labels require a separate `CONE_BATCH_TOKEN`; never
  reuse a Roblox API key or Discord webhook token for it.

Rotate a credential immediately if it is accidentally committed, pasted into an issue, or exposed in logs.

## Untrusted texture packs

Cone treats every uploaded archive and PNG as untrusted. The server bounds the
compressed request, ZIP entry count, declared expansion, per-entry extraction,
PNG dimensions, and total concurrent conversions. The Raspberry Pi deployment
also places nginx in front of Cone for request buffering, connection limits,
rate limits, and body timeouts. Keep both layers enabled on public deployments.
