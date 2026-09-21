# Cone

[![CI](https://github.com/qaustria/AutoPack-Go/actions/workflows/ci.yml/badge.svg)](https://github.com/qaustria/AutoPack-Go/actions/workflows/ci.yml)
[![Latest release](https://img.shields.io/github/v/release/qaustria/AutoPack-Go)](https://github.com/qaustria/AutoPack-Go/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-orange.svg)](LICENSE)

Cone ports Minecraft 1.8.9 texture packs to Roblox. It finds supported item and block textures, resizes them to 512×512, creates edge-expanded texture variants, builds greedy-meshed item geometry, uploads the assets through Roblox Open Cloud, and returns the compressed JSON used by the game.

![Cone web interface](docs/cone-ui.png)

## What it does

- Pure Go mesh generation, no Blender, `bpy`, or Python runtime.
- Greedy meshing that removes transparent background geometry.
- Consistent sword, pickaxe, axe, bow, potion, resource, wool, and utility-item mappings.
- Normal VP images plus edge-expanded texture variants.
- GLB model import with resolution to Roblox `MeshPart.MeshId` assets.
- Responsive local/public web interface with streamed conversion logs.
- Durable bbolt history for every successful port, indexed by Pack ID.
- Reusable image, mesh, GLB, texture-pack, and upload helpers in `utils`.
- Reusable port-history helpers in `packstore`.

## Requirements

- Go 1.22 or newer.
- A Roblox Open Cloud API key with:
  - **Assets: Read + Write**
  - **Asset Permissions: Write**
  - **Legacy Assets: Manage**
- IP restriction left off, unless every Cone server IP is explicitly allowed.

## Run the web interface

```bash
go run . --web
```

Open `http://127.0.0.1:8080`. The browser supplies a Roblox user ID and API key for each conversion. Request credentials are not written to server storage.

To listen on another address:

```bash
AUTOPACK_ADDR=0.0.0.0:8080 go run . --web
```

Use HTTPS whenever the interface is reachable over a network.

Successful web ports are appended to `data/cone.db`. Set
`CONE_DATABASE_PATH` to place the database elsewhere. Cone stores the Pack ID,
original filename, timestamp, and compressed output JSON; it does not store the
uploaded ZIP, Roblox API key, or preview image.

Cone listens directly with Go's HTTP server during local development. The Pi
deployment keeps Cone private on `127.0.0.1:8080`, with nginx on port 80 and
Cloudflare Tunnel targeting `http://localhost:80`. nginx bounds and
buffers uploads while leaving Cone's streamed conversion logs unbuffered.

Public uploads are limited to 128 MiB compressed, 256 MiB declared ZIP
expansion, 4,096 archive entries, and 16 megapixels per decoded texture. The Pi
allows one memory-heavy conversion at a time. Set `CONE_MAX_CONCURRENT_PORTS`
from 1 to 32 for a larger server; the default outside the Pi deployment is 2.

## Run from the command line

Copy the example configuration and fill in your own values:

```bash
cp .env_example.go .env.go
go run . /path/to/texture-pack.zip
```

You can use `ROBLOX_API_KEY` and `ROBLOX_USER_ID` environment variables instead. Cone writes `<pack>_cone.json` beside the input ZIP.

## Administrative batch import

The batch importer is a separate command and is not exposed anywhere in the
public website. It reads pack download links from a CSV file or a public Google
Sheet, submits them one at a time to the running local Cone service, and thereby
uses the same port-history database and Discord notification path as a normal
website conversion.

```bash
export CONE_BATCH_TOKEN="$(openssl rand -hex 32)"
go run . batch 'https://docs.google.com/spreadsheets/d/SHEET_ID/edit#gid=0'
```

The web server and batch command must receive the same `CONE_BATCH_TOKEN`.
Without it, administrative batch metadata is rejected so public clients cannot
forge batch progress in Discord notifications. Keep the token private and use a
separate value from every Roblox or Discord credential.

The sheet must be shared as **Anyone with the link → Viewer**, and download-link
cells must contain their complete `https://...` URL. `ROBLOX_API_KEY` and
`ROBLOX_USER_ID` select the Roblox account used for the import. The command
defaults to `http://127.0.0.1:8080/api/convert`; override that with
`CONE_BATCH_ENDPOINT` only when the trusted Cone service is elsewhere.
MediaFire file pages are resolved to their ZIP downloads automatically. A
MediaFire folder row recursively queues the ZIP files in that folder tree.

Progress is checkpointed in `data/batch-queue.json`, so rerunning the command
skips completed links and retries unfinished ones. Returned JSON is also copied
to the gitignored `batch-output/` directory. API keys are sent only in request
headers and are never written to the checkpoint or output files.

## Use the processing library

```go
package main

import (
    "image"

    "github.com/qaustria/AutoPack-Go/utils"
)

func build(img image.Image) ([]byte, error) {
    resized := utils.ResizeTexture(img)
    mesh, _, err := utils.BuildGreedyMesh(resized, utils.DefaultConfig())
    if err != nil {
        return nil, err
    }
    return utils.EncodeGeometryGLB(mesh)
}
```

`utils.EdgeExpand`, `utils.EncodeGLB`, `utils.UnzipTexturePack`, and `utils.NewAssetUploader` are also available for custom pipelines. The `packstore` package can open, save, count, and retrieve port-history records.

## Verify changes

```bash
go test -race ./...
go vet ./...
go build ./...
```

## Raspberry Pi

Tagged releases provide ARMv7 and ARM64 archives with SHA-256 checksums and all
required third-party license files. `deploy-pi.sh` downloads and verifies the
release matching its configured Cone version, so the Pi does not compile Go.

Store server-only values outside the checkout:

```bash
sudo install -d -m 0700 /etc/cone
sudoedit /etc/cone/cone.env
```

Example `/etc/cone/cone.env`:

```bash
CONE_DISCORD_WEBHOOK_URL=https://discord.com/api/webhooks/REPLACE_ME
CONE_BATCH_TOKEN=REPLACE_WITH_OPENSSL_RAND_HEX_32
```

Then install nginx once and deploy:

```bash
sudo apt-get update && sudo apt-get install -y nginx
cd ~/AutoPack-Go
git pull --ff-only origin master
./deploy-pi.sh
```

Set `CONE_VERSION` only when intentionally deploying a different published
version, for example `sudo CONE_VERSION=1.5.1 ./deploy-pi.sh`.

## Security

Never commit `.env.go`. See [SECURITY.md](SECURITY.md) for credential handling and private vulnerability reporting.

Release archives include dependency licenses described in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## License

[MIT](LICENSE)
