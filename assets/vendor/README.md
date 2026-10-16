# Vendored browser libraries

The app serves these files locally and embeds them in the Go binary. No runtime CDN or package registry access is required.

- `alpine-3.15.0.js`: Alpine.js 3.15.0, from the npm package's `dist/cdn.min.js`. MIT license in `alpine-LICENSE`.
- `alpine-persist-3.15.0.js`: the `@alpinejs/persist` 3.15.0 plugin, from the npm package's `dist/cdn.min.js`. MIT license, same as Alpine (`alpine-LICENSE`). Loaded before Alpine.
- `qrcode-1.4.4.js`: qrcode-generator 1.4.4, from the npm package's `qrcode.js`. MIT license in `qrcode-LICENSE` and the source header.

To update, install exact versions with Bun, copy the browser distributions here, retain the licenses, update script references, and bump the shell version in `assets/sw.js`. Browser assets must remain vendored.
