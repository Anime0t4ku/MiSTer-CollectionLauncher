# CollectionLauncher

CollectionLauncher is a controller-first collection launcher for MiSTer FPGA.

It lets you create custom game collections with their own wallpaper, optional logo, optional music, and selectable game artwork. Collections can be opened from the MiSTer Scripts menu or launched directly through NFC using Zaparoo.

## Installation

Download the latest release and extract it to the **root of your MiSTer SD card**.

The files will be installed under:

```text
/media/fat/Scripts/
├── CollectionLauncher.sh
└── .config/
    └── CollectionLauncher/
        ├── collection_launcher
        ├── Collections/
        └── tmp/
```

## Collection Structure

Collections are stored inside:

```text
/media/fat/Scripts/.config/CollectionLauncher/Collections/
```

Each collection gets its own folder inside that directory:

```text
/media/fat/Scripts/.config/CollectionLauncher/Collections/
└── ExampleCollection/
    ├── collection.json
    ├── wallpaper.png
    ├── logo.png
    ├── music.wav
    └── artwork/
        ├── game1.png
        └── game2.png
```

`logo.png` and `music.wav` are optional.

## Example collection.json

```json
{
  "id": "ExampleCollection",
  "title": "Example Collection",
  "wallpaper": "wallpaper.png",
  "logo": "logo.png",
  "music": "music.wav",
  "entries": [
    {
      "label": "Example Game",
      "artwork": "artwork/game1.png",
      "launch": {
        "system": "PSX",
        "path": "/media/fat/games/PSX/Example Game.chd"
      }
    },
    {
      "label": "Another Example",
      "artwork": "artwork/game2.png",
      "launch": {
        "system": "SNES",
        "path": "/media/fat/games/SNES/Another Example.sfc"
      }
    }
  ]
}
```

The `id` is used for direct launching. The collection folder name does not have to match the ID, although keeping them the same is recommended.

## Launching

Open the collection menu:

```bash
/media/fat/Scripts/CollectionLauncher.sh
```

Open a collection directly:

```bash
/media/fat/Scripts/CollectionLauncher.sh ExampleCollection
```

## NFC / Zaparoo

Write this text to the NFC tag:

```text
**mister.script:CollectionLauncher.sh ExampleCollection
```

Replace `ExampleCollection` with the `id` from `collection.json`.

Example:

```text
**mister.script:CollectionLauncher.sh CBC
```

Zaparoo only starts CollectionLauncher. CollectionLauncher creates the MGL and launches the selected game itself.

## Controls

```text
D-Pad    Navigate
A        Select / Launch
B        Back / Exit
```


## Assets

Recommended sizes:

```text
Wallpaper: 1920×1080
Artwork:   max 500×500 displayed
Logo:      max 600×200 displayed
```

Artwork and logos keep their aspect ratio. Smaller logos are not upscaled.

Current music support:

```text
WAV
```

## Game Scroller

A maximum of 3 games is shown at once.

```text
    [ Game 1 ] [ Game 2 ] [ Game 3 ]   >
```

Arrows only appear when more games exist on that side.

## Supported Systems

Current launch mappings include:

```text
PSX
Saturn
SNES
```

Game paths should use the full MiSTer path, for example:

```text
/media/fat/games/...
/media/fat/cifs/games/...
```

## Building From Source

CollectionLauncher is written in Go and targets Linux ARMv7.

### Linux / macOS

```bash
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 \
go build -trimpath -ldflags="-s -w" -o collection_launcher main.go
```

### Windows PowerShell

```powershell
$env:GOOS="linux"
$env:GOARCH="arm"
$env:GOARM="7"
$env:CGO_ENABLED="0"

go build -trimpath -ldflags="-s -w" -o collection_launcher main.go
```

Copy the resulting binary to:

```text
/media/fat/Scripts/.config/CollectionLauncher/collection_launcher
```

## Notes

CollectionLauncher does not include games/ROMs, artwork or music files.
