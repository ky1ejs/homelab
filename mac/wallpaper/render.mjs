// Render make.py's SVGs (the marks and text, on a transparent background) to
// PNG with the sharp that kylejs-site already installs.
// Usage: node render.mjs <kylejs-site checkout>
import { createRequire } from "node:module";
import path from "node:path";

const site = path.resolve(process.argv[2] ?? "../../../kylejs-site");
const sharp = createRequire(path.join(site, "package.json"))("sharp");
for (const theme of ["dark", "light"]) {
  await sharp(`mac-studio-${theme}.svg`, { limitInputPixels: false })
    .png()
    .toFile(`fg-${theme}.png`);
}
