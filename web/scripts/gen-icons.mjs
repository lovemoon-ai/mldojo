// Generates public/icons/icon.svg + icon-192.png + icon-512.png (no deps).
// Run: npm run icons
import { writeFileSync } from "node:fs";
import { deflateSync } from "node:zlib";

const BG = [0x18, 0x18, 0x1b];
// Tatami mark (see docs/brand/README.md): four mats pinwheeled around a
// half-mat, the classic 4.5-mat dojo floor. 512-unit coordinates.
const MAT = [0xfa, 0xfa, 0xfa];
const RUN = [0x39, 0x87, 0xe5];
const MATS = [
  { x: 112, y: 112, w: 184, h: 80, color: MAT },
  { x: 320, y: 112, w: 80, h: 184, color: MAT },
  { x: 216, y: 320, w: 184, h: 80, color: MAT },
  { x: 112, y: 216, w: 80, h: 184, color: MAT },
  { x: 216, y: 216, w: 80, h: 80, color: RUN },
];
const R = 12;

const hex = (c) => "#" + c.map((v) => v.toString(16).padStart(2, "0")).join("");
const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512">
  <rect width="512" height="512" rx="112" fill="${hex(BG)}"/>
${MATS.map((b) => `  <rect x="${b.x}" y="${b.y}" width="${b.w}" height="${b.h}" rx="${R}" fill="${hex(b.color)}"/>`).join("\n")}
</svg>
`;
writeFileSync(new URL("../public/icons/icon.svg", import.meta.url), svg);

// Bare mark (no tile) for light / dark UI backgrounds.
const mark = (mat, run) => `<svg xmlns="http://www.w3.org/2000/svg" viewBox="112 112 288 288">
${MATS.map((b) => `  <rect x="${b.x}" y="${b.y}" width="${b.w}" height="${b.h}" rx="${R}" fill="${b.color === RUN ? run : mat}"/>`).join("\n")}
</svg>
`;
writeFileSync(new URL("../public/icons/mark.svg", import.meta.url), mark("#18181b", "#2a78d6"));
writeFileSync(new URL("../public/icons/mark-dark.svg", import.meta.url), mark("#fafafa", "#3987e5"));

function inRoundRect(px, py, b) {
  if (px < b.x || px > b.x + b.w || py < b.y || py > b.y + b.h) return false;
  const cx = Math.min(Math.max(px, b.x + R), b.x + b.w - R);
  const cy = Math.min(Math.max(py, b.y + R), b.y + b.h - R);
  return (px - cx) ** 2 + (py - cy) ** 2 <= R * R;
}

const crcTable = Array.from({ length: 256 }, (_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});
const crc32 = (buf) => {
  let c = 0xffffffff;
  for (const b of buf) c = crcTable[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
};
const chunk = (type, data) => {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const td = Buffer.concat([Buffer.from(type), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(td));
  return Buffer.concat([len, td, crc]);
};

function png(size) {
  const SS = 4; // supersampling per axis
  const scale = 512 / size;
  const raw = Buffer.alloc(size * (size * 3 + 1));
  for (let y = 0; y < size; y++) {
    const row = y * (size * 3 + 1);
    raw[row] = 0; // filter: none
    for (let x = 0; x < size; x++) {
      const acc = [0, 0, 0];
      for (let sy = 0; sy < SS; sy++)
        for (let sx = 0; sx < SS; sx++) {
          const px = (x + (sx + 0.5) / SS) * scale;
          const py = (y + (sy + 0.5) / SS) * scale;
          const bar = MATS.find((b) => inRoundRect(px, py, b));
          const c = bar ? bar.color : BG; // full-bleed background (maskable-safe)
          acc[0] += c[0];
          acc[1] += c[1];
          acc[2] += c[2];
        }
      for (let i = 0; i < 3; i++) raw[row + 1 + x * 3 + i] = Math.round(acc[i] / (SS * SS));
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(size, 0);
  ihdr.writeUInt32BE(size, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // RGB
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk("IHDR", ihdr),
    chunk("IDAT", deflateSync(raw, { level: 9 })),
    chunk("IEND", Buffer.alloc(0)),
  ]);
}

for (const size of [192, 512]) {
  writeFileSync(new URL(`../public/icons/icon-${size}.png`, import.meta.url), png(size));
}
console.log("icons written to public/icons/");
