# kahut

A self-hosted, real-time quiz game. Hosts create quizzes or import JSON, invite players by room code or QR, and run timed questions with speed-based scoring and a live leaderboard. Players need only a nickname. SQLite preserves quizzes, answers, results, and active games across restarts.

## Run

Requires Go 1.27 or later. No frontend build is needed.

```sh
export SESSION_SIGNING_KEY="$(openssl rand -hex 32)"
AUTH_USERS='["alice:choose-a-password","bob:another-password"]' go run .
```

Open <http://localhost:8080>. Choose **Host a game**, sign in, and create a quiz or try the sample. Players open the invitation link or enter the room code on the homepage.

Pages use paths on the app’s subdomain: `/host` for the dashboard, `/host/quizzes/new` for creation, `/host/quizzes/{id}/edit` for a saved quiz editor, `/host/games/{code}` for results, and `/rooms/{code}` for invitations and live play. Refreshing a saved editor reloads its saved questions. Host-only links show login when needed and continue to the requested page after signing in.

Renaming a quiz updates its live and saved game titles, including games created before the rename. Changes to question content apply to new games; historical questions, answers, and scores are preserved.

`AUTH_USERS` is a required JSON array of `username:password` strings. Usernames are unique and case-sensitive; passwords may contain colons. Each account owns its quizzes, games, and results. Players still join with just a nickname. Restart the server to change accounts. Removing an account disables its host sessions; changing its password leaves existing sessions valid until logout or their 12-hour expiry.

`SESSION_SIGNING_KEY` is required and must contain at least 32 bytes. Generate a random value once, then save it in your server environment and reuse it across restarts. It is never stored in SQLite. Changing it invalidates existing host and player sessions.

Configuration: `PORT` (default `8080`), `ADDR` (used when `PORT` is unset), `DB_PATH` (default `./data/kahut.db`), and `DEBUG=1` to serve assets from disk. Keep the database directory persistent and private; it contains game data and session records. Multi-user support uses a fresh schema; point `DB_PATH` at a new database if you used the earlier single-host prototype.

API requests support all origins, including `Authorization`/JSON preflights, and authenticated WebSocket connections accept tunnel origins even when the proxy rewrites `Host`. Start the app locally, then point cloudflared or ngrok at `http://localhost:8080` and open the public HTTPS URL. Invitations and live connections use that public origin automatically. Authentication requires a configured account or a player session token; cookie-based credentials are not used.

Questions have 2–4 choices, one correct answer, and a 5–120 second timer. Correct answers earn 500–1,000 points based on server receipt time; wrong or missing answers earn zero. Players first see answer feedback; the host’s **Show leaderboard** action switches everyone to a dedicated ranking screen, and **Next question** returns them to the quiz. Late joiners wait for the next question. Interrupted questions retain their original deadlines.

The app supports English/Turkish, light/dark/system themes, and installation as a PWA. Language and appearance controls live in the Settings side menu. The offline shell opens without a network, but gameplay and quiz editing require the server. A root visit opens home; room paths and installed launches through `/resume` can restore a saved session. Previously shared query-based invitations still work and are converted to path URLs.

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
AUTH_USERS='["alice:choose-a-password","bob:another-password"]' docker compose up --build -d
```

Set `HOST_PORT` to change the published port. The non-root container stores its database in the `kahut-data` volume. Deploy one process per database; multiple replicas are not supported. Put an HTTPS reverse proxy in front for remote use and PWA installation, and enable WebSocket upgrades. No image or public release is published by this repository setup.

Back up the SQLite database using SQLite’s online backup facility or stop the container before copying the complete data directory, including any WAL files. Game history is retained until the data volume is removed.
