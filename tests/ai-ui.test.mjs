import test from 'node:test';
import assert from 'node:assert/strict';
import { chromium } from 'playwright';
import { spawn, execFileSync } from 'node:child_process';
import { createServer } from 'node:http';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

// A valid 1x1 PNG.
const PNG =
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==';

test(
  'host generates a question image from an editable prompt',
  { timeout: 120000 },
  async () => {
    const directory = await mkdtemp(join(tmpdir(), 'kahut-ai-ui-'));
    const binary = join(directory, 'kahut');
    const prompts = [];
    const upstream = createServer((request, response) => {
      let body = '';
      request.on('data', (chunk) => (body += chunk));
      request.on('end', () => {
        prompts.push(JSON.parse(body).messages[0].content);
        response.setHeader('Content-Type', 'application/json');
        response.end(
          JSON.stringify({
            choices: [
              {
                message: {
                  images: [{ image_url: { url: `data:image/png;base64,${PNG}` } }],
                },
              },
            ],
          }),
        );
      });
    });
    await new Promise((resolve) => upstream.listen(18084, '127.0.0.1', resolve));
    let server;
    let browser;
    try {
      execFileSync('go', ['build', '-o', binary, '.']);
      server = spawn(binary, {
        env: {
          ...process.env,
          AUTH_USERS: '["host:browser-test-password"]',
          SESSION_SIGNING_KEY: 'browser-test-signing-key-at-least-32-bytes',
          DB_PATH: join(directory, 'game.db'),
          PORT: '18083',
          OPENROUTER_API_KEY: 'test-key',
          OPENROUTER_MODEL: 'test/model',
          OPENROUTER_BASE_URL: 'http://127.0.0.1:18084',
        },
        stdio: 'ignore',
      });
      const base = 'http://127.0.0.1:18083';
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
      const page = await (await browser.newContext()).newPage();
      const errors = [];
      page.on('pageerror', (error) => errors.push(error.message));
      await page.addInitScript(() =>
        localStorage.setItem('kahut.imagePrompt', 'Flat vector'),
      );
      await page.goto(`${base}/host`);
      await page.getByLabel('Username', { exact: true }).fill('host');
      await page.getByLabel('Password', { exact: true }).fill('browser-test-password');
      await page.getByRole('button', { name: 'Sign in', exact: true }).click();
      await page.waitForURL(`${base}/host`);
      await page.getByRole('button', { name: '+ New quiz', exact: true }).first().click();
      const editor = page.locator('.question-editor').first();
      await editor.getByLabel('What’s the question?').fill('Capital of France?');
      const inputs = await editor.locator('.choice-editor input[type=text]').all();
      await inputs[0].fill('Paris');
      await inputs[1].fill('Rome');

      await editor.getByRole('button', { name: '✨ Generate' }).click();
      const box = editor.locator('.image-generate textarea');
      assert.equal(
        await box.inputValue(),
        'Flat vector\n\nCapital of France?\n\nAnswer options: Paris; Rome',
      );
      await box.fill('A watercolor of the Eiffel Tower');
      await editor
        .locator('.image-generate')
        .getByRole('button', { name: '✨ Generate' })
        .click();
      await editor.locator('.image-preview img').waitFor();
      assert.deepEqual(prompts, ['A watercolor of the Eiffel Tower']);
      assert.deepEqual(errors, []);
    } finally {
      await browser?.close();
      server?.kill();
      upstream.close();
      await rm(directory, { recursive: true, force: true });
    }
  },
);
