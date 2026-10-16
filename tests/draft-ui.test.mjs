import test from 'node:test';
import assert from 'node:assert/strict';
import { chromium } from 'playwright';
import { spawn, execFileSync } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

test(
  'editor blocks a second empty question and restores unsaved changes after a refresh',
  { timeout: 120000 },
  async () => {
    const directory = await mkdtemp(join(tmpdir(), 'kahut-draft-ui-'));
    const binary = join(directory, 'kahut');
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
          PORT: '18085',
        },
        stdio: 'ignore',
      });
      const base = 'http://127.0.0.1:18085';
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
      await page.goto(`${base}/host`);
      await page.getByLabel('Username', { exact: true }).fill('host');
      await page.getByLabel('Password', { exact: true }).fill('browser-test-password');
      await page.getByRole('button', { name: 'Sign in', exact: true }).click();
      await page.waitForURL(`${base}/host`);
      await page.getByRole('button', { name: '+ New quiz', exact: true }).first().click();
      await page.waitForURL(`${base}/host/quizzes/new`);

      const add = page.getByRole('button', { name: '+ Add question' });
      const editors = page.locator('.question-editor');
      // A new quiz starts with one empty question, so adding is blocked.
      assert.equal(await add.isDisabled(), true);
      await editors.first().getByLabel('What’s the question?').fill('First?');
      assert.equal(await add.isDisabled(), false);
      await add.click();
      assert.equal(await editors.count(), 2);
      assert.equal(await add.isDisabled(), true);

      await page.getByLabel('Quiz title').fill('Draft quiz');
      const inputs = await editors
        .first()
        .locator('.choice-editor input[type=text]')
        .all();
      await inputs[0].fill('Yes');

      // Wait for the plugin to write the draft, then refresh.
      await page.waitForFunction(() =>
        localStorage.getItem('kahut.draft')?.includes('Draft quiz'),
      );
      await page.reload();
      await page.getByLabel('Quiz title').waitFor();
      assert.equal(await page.getByLabel('Quiz title').inputValue(), 'Draft quiz');
      assert.equal(await editors.count(), 2);
      assert.equal(
        await editors.first().getByLabel('What’s the question?').inputValue(),
        'First?',
      );
      assert.equal(
        await editors
          .first()
          .locator('.choice-editor input[type=text]')
          .first()
          .inputValue(),
        'Yes',
      );
      await page.getByText('Restored your unsaved changes.').waitFor();

      // Leaving and discarding forgets the draft.
      page.once('dialog', (dialog) => dialog.accept());
      await page
        .getByRole('button', { name: /back|library/i })
        .first()
        .click();
      await page.waitForFunction(() => localStorage.getItem('kahut.draft') === 'null');
      assert.deepEqual(errors, []);
    } finally {
      await browser?.close();
      server?.kill();
      await rm(directory, { recursive: true, force: true });
    }
  },
);
