# Gressio Docker Overlay

This repository keeps the base Docker files close to upstream `new-api` and
adds Gressio-specific runtime behavior through compose overlays in the
`transwork/` folder.

The overlay is responsible for:

- building a custom image from the merged source tree
- mounting the Google Cloud service account key at runtime
- providing the Gressio ASR environment variables

The base compose file remains PostgreSQL-based and upstream-like.

## Files

- `../docker-compose.yml`: base production stack (`new-api` + `postgres` + `redis`)
- `docker-compose.transwork.yml`: production Gressio overlay
- `../docker-compose.dev.yml`: base development stack
- `docker-compose.transwork.dev.yml`: development Gressio overlay
- `transwork.env.example`: example environment file for the overlays

## Production

1. Copy `transwork/transwork.env.example` to `transwork/transwork.env`
2. Update:
   - `IMAGE_TAG`
   - `TRANSWORK_GCS_KEY_PATH`
   - `GCS_BUCKET_NAME`
   - `ELEVENLABS_API_KEY`
3. Start the stack (must run at transwork-server root location):

```bash
docker compose `
  --env-file transwork/transwork.env `
  -f docker-compose.yml `
  -f transwork/docker-compose.transwork.yml `
  up -d --build
```

Stop:

```bash
docker compose `
  --env-file transwork/transwork.env `
  -f docker-compose.yml `
  -f transwork/docker-compose.transwork.yml `
  down
```

## Development

The base dev compose runs the backend in Docker and expects the frontend dev
server to run on the host.

Start the backend stack:

```bash
docker compose \
  --env-file transwork/transwork.env \
  -f docker-compose.dev.yml \
  -f transwork/docker-compose.transwork.dev.yml \
  up -d --build
```

Run the frontend dev server separately using the upstream workflow for the
active web app.

Stop:

```bash
docker compose \
  --env-file transwork/transwork.env \
  -f docker-compose.dev.yml \
  -f transwork/docker-compose.transwork.dev.yml \
  down
```

## Notes

- The GCS key is mounted at `/app/key.json` and the server reads it via
  `GCS_CREDENTIALS_PATH=/app/key.json`.
- Docker Compose resolves the overlay build context and bind-mount paths from
  the base compose file in the repository root, so `TRANSWORK_GCS_KEY_PATH`
  should normally be written relative to the repo root, for example `./key.json`.
- Do not bake `key.json` into the image.
- The root `Dockerfile` already builds the merged server source, including the
  `transwork/` package, so the overlay only needs to switch the service from an
  official image to a local source build.
