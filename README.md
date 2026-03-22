# ai-gateway

`ai-gateway` is an air-gapped friendly control plane and OpenAI-compatible gateway for teams that already run many vLLM servers and need one place to manage them.

## What it does

- Exposes a single `/v1/chat/completions` endpoint.
- Lets clients call logical model names instead of raw vLLM hostnames.
- Routes requests to registered vLLM backends.
- Runs periodic health checks and shows backend health in an admin UI.
- Issues bearer API keys for northbound access.
- Tracks recent proxied requests.
- Imports and exports gateway configuration as JSON.

## Why Go

This project is implemented in Go because it is a strong operational fit for an internal gateway:

- Single static binary deployment is easy to move into a closed network.
- It handles proxying and streaming well.
- The admin UI can be embedded directly into the executable.
- Docker and non-Docker deployment are both straightforward.

## Quick start

Run the binary:

```bash
AI_GATEWAY_ADMIN_USERNAME=admin \
AI_GATEWAY_ADMIN_PASSWORD=change-me \
./ai-gateway
```

Then open `http://localhost:8080/admin`.

## Environment variables

- `AI_GATEWAY_BIND_ADDR`: Listen address. Default `:8080`
- `AI_GATEWAY_DATA_DIR`: Data directory. Default `./data`
- `AI_GATEWAY_DATABASE_PATH`: Optional explicit SQLite path. Defaults to `<AI_GATEWAY_DATA_DIR>/ai-gateway.db`
- `AI_GATEWAY_TITLE`: UI title. Default `AI Gateway`
- `AI_GATEWAY_ADMIN_USERNAME`: Optional admin UI basic auth username
- `AI_GATEWAY_ADMIN_PASSWORD`: Optional admin UI basic auth password
- `AI_GATEWAY_ALLOW_ANONYMOUS`: Allow unauthenticated `/v1/*` access. Default `false`
- `AI_GATEWAY_UPSTREAM_INSECURE_SKIP_VERIFY`: Skip TLS verification for upstream backends. Default `false`
- `AI_GATEWAY_REQUEST_TIMEOUT`: Upstream request timeout. Default `120s`
- `AI_GATEWAY_HEALTHCHECK_INTERVAL`: Background health probe interval. Default `30s`

## Admin UI

The admin UI is available at `/admin` and supports:

- Creating and updating logical models
- Registering existing vLLM backends
- Running health probes
- Issuing and revoking API keys
- Exporting and importing configuration
- Inspecting recent request logs

## Air-gapped delivery

Use the packaging script to build the transport files:

```powershell
.\scripts\build-airgap-bundle.ps1 -Version 0.1.3
```

The script produces:

- `ai-gateway_0.1.3_linux_amd64.tar.gz`
- `ai-gateway_0.1.3_docker-image.tar`
- `ai-gateway_0.1.3_source.zip`
- `ai-gateway_0.1.3_airgap-bundle.zip`
- `ai-gateway_0.1.3_checksums.txt`

The air-gap bundle contains the binary, Docker image tar, compose file, env example, helper import scripts, and the source snapshot.

## Notes

- This MVP intentionally focuses on `/v1/chat/completions` first.
- Existing vLLM servers stay where they are; the gateway manages them instead of replacing them.
- Configuration export includes backend upstream API keys, so treat exported JSON as a sensitive file.
