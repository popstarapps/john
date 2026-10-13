/**
 * Renders the share card from the dark logo in src/assets.
 *
 *   node scripts/build-logos.mjs
 *
 * Everything else the site shows is SVG and needs no build step: the logos in
 * src/assets, and the favicons in public/ (the hexagon alone, with a square
 * viewBox). The console ships copies of the same files.
 *
 * The share card is the one asset that has to be a raster — Slack, Teams and
 * the social sites don't render SVG unfurls. It can't be transparent either,
 * since they composite it onto their own ground, which is not always the one
 * you expect. So the lockup sits on the site's dark background at 60% of the
 * card width.
 *
 * Output:
 *
 *   public/og.png   1200×630 share card
 */
import sharp from "sharp";

const landing = new URL("../", import.meta.url);

const OG = { w: 1200, h: 630, bg: { r: 10, g: 17, b: 32, alpha: 1 } }; // --bg-deep, dark

const card = await sharp(new URL("src/assets/logo-dark.svg", landing).pathname, { density: 300 })
  .resize({ width: Math.round(OG.w * 0.6), fit: "inside" })
  .png()
  .toBuffer();
const og = await sharp({
  create: { width: OG.w, height: OG.h, channels: 4, background: OG.bg },
})
  .composite([{ input: card, gravity: "centre" }])
  .png({ compressionLevel: 9 })
  .toFile(new URL("public/og.png", landing).pathname);
console.log(
  `  ${"public/og.png".padEnd(28)} ${og.width}×${og.height}  ${(og.size / 1024).toFixed(1)} kB`,
);
