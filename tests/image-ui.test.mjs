import test from 'node:test';
import assert from 'node:assert/strict';
import { chromium } from 'playwright';
import { spawn, execFileSync } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

// A valid 1x1 PNG.
const PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64',
);

test(
  'host adds question images by picking, pasting and dropping',
  { timeout: 120000 },
  async () => {
    const directory = await mkdtemp(join(tmpdir(), 'kahut-image-ui-'));
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
          PORT: '18082',
        },
        stdio: 'ignore',
      });
      const base = 'http://127.0.0.1:18082';
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
      await page.getByLabel('Quiz title').fill('Pictures');
      const editors = page.locator('.question-editor');
      await editors.nth(0).getByLabel('What’s the question?').fill('Question one');
      for (const editor of await editors.all()) {
        for (const [j, input] of (
          await editor.locator('.choice-editor input[type=text]').all()
        ).entries())
          await input.fill(`Choice ${j}`);
      }

      // Pick a file.
      await editors
        .nth(0)
        .locator('input[type=file]')
        .setInputFiles({ name: 'a.png', mimeType: 'image/png', buffer: PNG });
      await editors.nth(0).locator('.image-preview img').waitFor();

      // Clicking the preview enlarges it in the lightbox; a click closes it.
      await editors.nth(0).locator('.image-zoom').click();
      await page.locator('dialog.lightbox[open] img').waitFor();
      assert.match(
        await page.locator('dialog.lightbox img').getAttribute('src'),
        /^\/api\/images\//,
      );
      await page.locator('dialog.lightbox').click();
      await page.locator('dialog.lightbox[open]').waitFor({ state: 'detached' });

      // Unsupported and oversized files are rejected before upload.
      await page.getByRole('button', { name: 'Add question', exact: false }).click();
      const second = editors.nth(1);
      await second.getByLabel('What’s the question?').fill('Question two');
      for (const [j, input] of (
        await second.locator('.choice-editor input[type=text]').all()
      ).entries())
        await input.fill(`Choice ${j}`);
      await second.locator('input[type=file]').setInputFiles({
        name: 'a.svg',
        mimeType: 'image/svg+xml',
        buffer: Buffer.from('<svg/>'),
      });
      await page.getByText('Use a PNG, JPEG, GIF or WebP image.').waitFor();
      await second.locator('input[type=file]').setInputFiles({
        name: 'big.png',
        mimeType: 'image/png',
        buffer: Buffer.alloc(4 * 1024 * 1024 + 1),
      });
      await page.getByText('The image must be 4 MB or smaller.').waitFor();
      assert.equal(await second.locator('.image-preview').count(), 0);

      // Paste into the question text.
      await second.getByLabel('What’s the question?').focus();
      await page.evaluate(() => {
        const data = new DataTransfer();
        data.items.add(new File([new Uint8Array([1])], 'x.png', { type: 'image/png' }));
        const event = new ClipboardEvent('paste', {
          clipboardData: data,
          bubbles: true,
          cancelable: true,
        });
        document.activeElement.dispatchEvent(event);
      });
      // The tiny fake is not a real image, so the server refuses it.
      await page.getByText('image must be PNG, JPEG, GIF or WebP').waitFor();

      // Drop a real image.
      const bytes = [...PNG];
      await second.evaluate(async (el, bytes) => {
        const data = new DataTransfer();
        data.items.add(new File([new Uint8Array(bytes)], 'd.png', { type: 'image/png' }));
        el.dispatchEvent(
          new DragEvent('drop', { dataTransfer: data, bubbles: true, cancelable: true }),
        );
      }, bytes);
      await second.locator('.image-preview img').waitFor();

      // Remove, then save and reload to prove persistence.
      await second.getByRole('button', { name: 'Remove image' }).click();
      assert.equal(await second.locator('.image-preview').count(), 0);
      await second
        .locator('input[type=file]')
        .setInputFiles({ name: 'again.png', mimeType: 'image/png', buffer: PNG });
      await second.locator('.image-preview img').waitFor();
      await page.getByRole('button', { name: 'Save quiz', exact: true }).last().click();
      await page.getByText('Saved', { exact: false }).first().waitFor();
      await page.getByRole('button', { name: 'Edit', exact: true }).click();
      await page.reload();
      await editors.nth(1).locator('.image-preview img').waitFor();
      const loaded = await editors
        .nth(0)
        .locator('.image-preview img')
        .evaluate((img) => img.complete && img.naturalWidth);
      assert.equal(loaded, 1);
      assert.deepEqual(errors, []);
    } finally {
      await browser?.close();
      server?.kill();
      await rm(directory, { recursive: true, force: true });
    }
  },
);
