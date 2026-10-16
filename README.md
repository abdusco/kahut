# kahut

A self-hosted, real-time quiz game. Hosts create quizzes or import JSON, invite players by room code or QR, and run timed questions with speed-based scoring and a live leaderboard. Players need only a nickname. SQLite preserves quizzes, answers, results, and active games across restarts.

## Run

Requires Go 1.27 or later. No frontend build is needed.

```sh
HOST_PASSWORD='choose-a-host-password' go run .
```

Open <http://localhost:8080>. Choose **Host a game**, sign in, and create a quiz or try the sample. Players open the invitation link or enter the room code on the homepage.

Configuration: `PORT` (default `8080`), `ADDR` (used when `PORT` is unset), `DB_PATH` (default `./data/kahut.db`), and `DEBUG=1` to serve assets from disk. Keep the database directory persistent and private; it contains game data and session signing material. Changing `HOST_PASSWORD` requires a restart; existing host sessions remain valid until logout or their 12-hour expiry.

API requests support all origins, including `Authorization`/JSON preflights, and authenticated WebSocket connections accept tunnel origins even when the proxy rewrites `Host`. Start the app locally, then point cloudflared or ngrok at `http://localhost:8080` and open the public HTTPS URL. Invitations and live connections use that public origin automatically. Authentication still requires the host password or a player session token; cookie-based credentials are not used.

Questions have 2–4 choices, one correct answer, and a 5–120 second timer. Correct answers earn 500–1,000 points based on server receipt time; wrong or missing answers earn zero. Hosts advance through results and leaderboards. Late joiners wait for the next question. Interrupted questions retain their original deadlines.

The app supports English/Turkish, light/dark/system themes, and installation as a PWA. The offline shell opens without a network, but gameplay and quiz editing require the server. A root visit opens home; invitation links and installed launches can restore a saved session.

## Quiz JSON

Use **Export** for a portable file or **Import JSON** to open the import dialog. Paste JSON text, paste/drop a JSON file, or select a file to populate the textarea, then review and import. The dialog includes a copyable prompt for generating quizzes with an AI of your choice. Imports are limited to 1 MB and 100 questions. `correctIndex` starts at zero; omitted or zero `timeLimitSeconds` defaults to 20.

```json
{
  "version": 1,
  "title": "A quick quiz",
  "questions": [
    {
      "text": "Which planet is known as the Red Planet?",
      "choices": ["Mars", "Venus", "Jupiter", "Saturn"],
      "correctIndex": 0,
      "timeLimitSeconds": 20
    }
  ]
}
```

## Test and format

```sh
go test -race ./...
go vet ./...
bun install --frozen-lockfile
bun run test
bunx playwright install chromium
bun run test:ui
gofmt -w *.go
bunx prettier --write assets scripts tests package.json .prettierrc.json README.md compose.yaml
```

Browser dependencies are pinned and vendored with licenses under `assets/vendor/`; the running app never uses a CDN. Bun dependencies are development tools and the sources for vendored files. `bun run icons` regenerates PNG installation icons from SVG with Chromium.

## Deploy

```sh
HOST_PASSWORD='choose-a-host-password' docker compose up --build -d
```

Set `HOST_PORT` to change the published port. The non-root container stores its database in the `kahut-data` volume. Deploy one process per database; multiple replicas are not supported. Put an HTTPS reverse proxy in front for remote use and PWA installation, and enable WebSocket upgrades. No image or public release is published by this repository setup.

Back up the SQLite database using SQLite’s online backup facility or stop the container before copying the complete data directory, including any WAL files. Game history is retained until the data volume is removed.
