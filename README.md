# kahut

A self-hosted, real-time quiz game. Hosts create quizzes, players join with a room code or QR code and a nickname, and everyone races through timed questions with a live leaderboard.

## Quick start

Requires Go 1.27 or later. No frontend build is needed.

```sh
export SESSION_SIGNING_KEY="$(openssl rand -hex 32)"
AUTH_USERS='["alice:choose-a-password"]' go run .
```

Open <http://localhost:8080>, choose **Host a game**, sign in, and create a quiz or try the sample.

## How it works

**Hosts**

1. Sign in, then create a quiz (or import one, see [Quiz JSON](#quiz-json)).
2. Start a game. Share the room code, link or QR code.
3. Run the questions. Use **Show leaderboard** to show the ranking and **Next question** to continue.
4. Past games and their results stay on the dashboard.

**Players** open the link or enter the room code on the homepage, pick a nickname, and answer on their own device. Late joiners wait for the next question.

**Questions** have 2–4 choices, one correct answer, and a 5–120 second timer. A correct answer earns 500–1,000 points, and faster answers earn more. Wrong or missing answers earn zero.

**Also:** English and Turkish, light/dark/system themes, and installation as an app (PWA). Language and theme are in the Settings menu. Gameplay and quiz editing need a connection to the server.

## Question images

Each question can have one image. Choose **Add image**, drop a file on the question, or paste an image into the question text. PNG, JPEG, GIF and WebP up to 4 MB work. SVG does not. Click an image to enlarge it.

Games keep their images when the quiz is edited or deleted. Images that no quiz or game uses are removed after 24 hours.

Anyone with an image link can view the image, so don't upload sensitive pictures. Quiz export leaves images out.

### Generate images with AI

Set `OPENROUTER_API_KEY` and `OPENROUTER_MODEL` to show a **✨ Generate** button next to **Add image**. Use an image model from [OpenRouter](https://openrouter.ai/models?output_modalities=image). `recraft/recraft-v4.1-flash` was tested and cost about 0.7¢ per image.

1. Click **Generate**. The prompt starts with your default image prompt and the question text, and you can edit it.
2. Click the result to enlarge it, then choose **Use this image**, **Regenerate** or **Cancel**.

Set the default image prompt (for example, an art style) in the Settings menu. It is saved in your browser.

Things to know:

- The server's key is shared by every account, so all generation is billed to one OpenRouter account. Each host can run one generation at a time.
- The prompt is sent to OpenRouter and the model provider.
- Generated PNGs are converted to JPEG to save space.

## Quiz JSON

Use **Export** to save a quiz, or **Import JSON** to load one. You can paste text, drop a file, or pick a file. The import dialog has a prompt you can give to an AI to write a quiz for you. Imports are limited to 1 MB and 100 questions.

`correctIndex` starts at zero. A missing or zero `timeLimitSeconds` becomes 20.

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

## Configuration

| Variable              | Required | Description                                                                                                                                                |
| --------------------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `AUTH_USERS`          | yes      | JSON array of `username:password` strings, for example `["alice:pw1","bob:pw2"]`. Each account owns its own quizzes and games. Restart to change accounts. |
| `SESSION_SIGNING_KEY` | yes      | At least 32 random bytes. Keep the same value across restarts. Changing it signs everyone out.                                                             |
| `PORT`                | no       | Port to listen on. Default `8080`.                                                                                                                         |
| `DB_PATH`             | no       | SQLite file. Default `./data/kahut.db`. Keep it private and back it up.                                                                                    |
| `OPENROUTER_API_KEY`  | no       | Enables AI image generation (with `OPENROUTER_MODEL`).                                                                                                     |
| `OPENROUTER_MODEL`    | no       | Image model used for generation.                                                                                                                           |

## Deploy

```sh
AUTH_USERS='["alice:choose-a-password"]' docker compose up --build -d
```

- Set `HOST_PORT` to change the published port. The database lives in the `kahut-data` volume.
- Run one process per database. Replicas are not supported.
- For remote use and app installation, put an HTTPS reverse proxy in front and enable WebSocket upgrades.
- To try it publicly from your machine, point cloudflared or ngrok at `http://localhost:8080`. Invitations use the public address automatically.
- Back up the database with SQLite's online backup, or stop the container before copying the whole data directory (including WAL files).

## Development

```sh
go test -race ./...
go vet ./...
bun install --frozen-lockfile
bun run test
bunx playwright install chromium
bun run test:ui
gofmt -w *.go
bun run format
```

Frontend dependencies are vendored under `assets/vendor/`. The app never loads anything from a CDN. Set `DEBUG=1` to serve assets from disk while developing.
