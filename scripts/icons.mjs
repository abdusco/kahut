// Development-only: rasterize the vector icons directly at their target sizes.
import { chromium } from 'playwright';
import { readFile, writeFile } from 'node:fs/promises';

const browser = await chromium.launch({
  headless: true,
  channel: process.env.PLAYWRIGHT_CHANNEL || undefined,
});
try {
  const page = await browser.newPage();
  for (const [source, target, size] of [
    ['icon.svg', 'icon-192.png', 192],
    ['icon.svg', 'icon-512.png', 512],
    ['maskable.svg', 'maskable-512.png', 512],
  ]) {
    const svg = await readFile(
      new URL(`../assets/icons/${source}`, import.meta.url),
      'utf8',
    );
    const png = await page.evaluate(
      async ({ svg, size }) => {
        const image = new Image();
        image.src = `data:image/svg+xml;base64,${btoa(svg)}`;
        await image.decode();
        const canvas = document.createElement('canvas');
        canvas.width = canvas.height = size;
        canvas.getContext('2d').drawImage(image, 0, 0, size, size);
        return canvas.toDataURL('image/png').split(',')[1];
      },
      { svg, size },
    );
    await writeFile(
      new URL(`../assets/icons/${target}`, import.meta.url),
      Buffer.from(png, 'base64'),
    );
  }
} finally {
  await browser.close();
}
