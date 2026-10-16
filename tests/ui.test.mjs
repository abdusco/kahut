import test from 'node:test';
import assert from 'node:assert/strict';
import { chromium } from 'playwright';
import { spawn, execFileSync } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

// Reproducible browser smoke test. Live verification during development can also
// be performed with Chrome MCP using the same host/player flow.
test(
  'host imports a quiz, player answers, and results survive a reload',
  { timeout: 120000 },
  async () => {
    const directory = await mkdtemp(join(tmpdir(), 'kahut-ui-'));
    const binary = join(directory, 'kahut');
    let server;
    let browser;
    try {
      execFileSync('go', ['build', '-o', binary, '.']);
      server = spawn(binary, {
        env: {
          ...process.env,
          HOST_PASSWORD: 'browser-test-password',
          DB_PATH: join(directory, 'game.db'),
          PORT: '18081',
        },
        stdio: 'ignore',
      });
      const base = 'http://127.0.0.1:18081';
      for (let attempt = 0; ; attempt++) {
        try {
          if ((await fetch(base)).ok) break;
        } catch {
          /* Wait for the listener. */
        }
        if (attempt >= 100) throw new Error('test server did not start');
        await delay(100);
      }
      browser = await chromium.launch({
        channel: process.env.PLAYWRIGHT_CHANNEL || undefined,
      });
      const hostContext = await browser.newContext();
      const playerContext = await browser.newContext({
        viewport: { width: 320, height: 740 },
      });
      const host = await hostContext.newPage();
      const player = await playerContext.newPage();
      const errors = [];
      host.on('pageerror', (error) => errors.push(error.message));
      player.on('pageerror', (error) => errors.push(error.message));
      await host.goto(base);
      assert.equal(await host.locator('header select').count(), 0);
      await host.getByRole('button', { name: 'Settings', exact: true }).click();
      const settings = host.getByRole('dialog', { name: 'Settings', exact: true });
      await settings.getByLabel('Appearance').selectOption('light');
      assert.equal(await host.locator('html').getAttribute('data-theme'), 'light');
      await settings.getByRole('button', { name: 'Dismiss', exact: true }).click();
      await settings.waitFor({ state: 'hidden' });
      await host.getByRole('button', { name: 'Host a game', exact: true }).click();
      await host.getByLabel('Host password').fill('browser-test-password');
      await host.getByRole('button', { name: 'Sign in', exact: true }).click();
      await host.waitForURL(`${base}/host`);
      await host.reload();
      await host
        .getByRole('heading', { name: 'Your quiz collection.', exact: true })
        .waitFor();
      assert.equal(new URL(host.url()).pathname, '/host');
      await host.getByRole('button', { name: 'Import JSON', exact: true }).click();
      const dialog = host.getByRole('dialog');
      assert.equal(await dialog.isVisible(), true);
      assert.match(
        await dialog.getByLabel('Question-generation prompt').inputValue(),
        /correctIndex/,
      );
      await dialog.getByLabel('Quiz JSON', { exact: true }).fill('{broken');
      await dialog.getByRole('button', { name: 'Import quiz', exact: true }).click();
      await host.waitForFunction(
        () => document.querySelector('dialog .error').innerText.length > 0,
      );
      const quiz = {
        version: 1,
        title: 'Hat trick',
        questions: [
          {
            text: 'What does Hut mean?',
            choices: ['Hat', 'Shoe'],
            correctIndex: 0,
            timeLimitSeconds: 120,
          },
        ],
      };
      await dialog.getByLabel('Load a JSON file into the editor').setInputFiles({
        name: 'quiz.json',
        mimeType: 'application/json',
        buffer: Buffer.from(JSON.stringify(quiz)),
      });
      await host.waitForFunction(() =>
        document.getElementById('import-json').value.includes('Hat trick'),
      );
      await dialog.getByRole('button', { name: 'Import quiz', exact: true }).click();
      await host.getByRole('button', { name: 'Edit', exact: true }).click();
      assert.match(new URL(host.url()).pathname, /^\/host\/quizzes\/[^/]+\/edit$/);
      await host.reload();
      await host.getByRole('heading', { name: 'Edit quiz', exact: true }).waitFor();
      assert.equal(await host.getByLabel('Quiz title').inputValue(), 'Hat trick');
      assert.equal(await host.locator('.choice-editor.is-correct').count(), 1);
      await host.getByRole('radio', { name: 'Correct answer 2', exact: true }).check();
      assert.equal(
        await host.locator('.choice-editor.is-correct input[type="text"]').inputValue(),
        'Shoe',
      );
      await host.getByRole('button', { name: 'Save quiz', exact: true }).click();
      await host.getByRole('button', { name: 'Edit', exact: true }).click();
      assert.equal(
        await host.locator('.choice-editor.is-correct input[type="text"]').inputValue(),
        'Shoe',
      );
      await host.getByRole('radio', { name: 'Correct answer 1', exact: true }).check();
      assert.equal(
        await host.locator('.choice-editor.is-correct .correct-choice-label').isVisible(),
        true,
      );
      await host.getByRole('button', { name: 'Save quiz', exact: true }).click();
      await host.getByRole('button', { name: 'Host', exact: true }).click();
      await host.waitForURL(/\/rooms\/[A-Z0-9]{8}$/);
      await player.goto(host.url());
      await player.getByLabel('Your nickname').fill('Ada');
      await player.getByRole('button', { name: 'Join a game →', exact: true }).click();
      await host.getByRole('button', { name: 'Start the game →', exact: true }).click();
      await player.getByRole('heading', { name: 'What does Hut mean?' }).waitFor();
      assert.equal(await player.locator('.stage-scoreboard').isVisible(), false);
      await player.getByRole('button', { name: '▲ Hat', exact: true }).click();
      await player.locator('.personal-result').waitFor();
      assert.equal(await player.locator('.stage-scoreboard').isVisible(), false);
      assert.equal(
        await player.getByRole('heading', { name: 'What does Hut mean?' }).isVisible(),
        true,
      );
      await host.getByRole('button', { name: 'Show leaderboard →', exact: true }).click();
      await player
        .getByRole('heading', { name: 'The leaderboard', exact: true })
        .waitFor();
      await player.locator('.question-stage').waitFor({ state: 'hidden' });
      assert.equal(await player.locator('.personal-standing').isVisible(), true);
      await host
        .getByRole('button', { name: 'Reveal the podium →', exact: true })
        .click();
      await player.getByRole('heading', { name: 'Meet your quiz champions.' }).waitFor();
      for (const width of [320, 390, 720, 1000]) {
        await player.setViewportSize({ width, height: 900 });
        assert.equal(
          await player.evaluate(() => document.documentElement.scrollWidth <= innerWidth),
          true,
        );
      }
      await player.reload();
      await player.getByRole('heading', { name: 'Meet your quiz champions.' }).waitFor();
      await player.getByRole('link', { name: 'kahut home' }).click();
      await player.getByRole('heading', { name: 'Join a game', exact: true }).waitFor();
      assert.equal(
        await player.getByText('Good questions. Great company.', { exact: true }).count(),
        0,
      );
      assert.deepEqual(errors, []);
    } finally {
      await browser?.close();
      if (server) {
        const exited = new Promise((resolve) => server.once('exit', resolve));
        server.kill('SIGTERM');
        await exited;
      }
      await rm(directory, { recursive: true, force: true });
    }
  },
);
